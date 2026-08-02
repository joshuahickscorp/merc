//! Merc-owned deterministic video synthesizer.
//!
//! This is NOT a diffusion runtime and NOT buyer-routable. It exists so the
//! governed `video_generation` lane (decomposition, byte-exact verification,
//! duration-sum, settlement) can be exercised under one pinned engine build.
//! There is no pinnable open-licensed video weight set in-tree; inventing one
//! would be a false claim.

use std::time::Instant;

use async_trait::async_trait;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use crate::executor::{JobOutput, JobRunner, RunError};
use crate::pool::ModelPool;
use crate::types::{
    BenchResult, InputRef, JobConstraints, JobManifest, JobType, ModelKind, ModelRef, OutputRef,
    ServiceTier, VerificationPolicy, WorkerCapability,
};

pub const VIDEO_GENERATION_MODEL_REF: &str = "merc-video-synth-v1";
const MAX_PROMPT_BYTES: usize = 64 << 10;
const VIDEO_BACKEND: &str = "merc_video_synth_v1";

#[derive(Debug, Clone, Copy)]
struct VideoSpec {
    width: u32,
    height: u32,
    fps: u32,
    duration_secs: u32,
}

#[derive(Debug, Deserialize)]
struct VideoPromptDoc {
    prompt: String,
    #[serde(default)]
    seed: i64,
}

#[derive(Debug, Clone, Copy)]
struct SegmentAssignment {
    ordinal: u32,
    unit_count: u32,
}

fn video_spec(manifest: &JobManifest) -> Result<VideoSpec, RunError> {
    if manifest.model.kind != ModelKind::Builtin
        || manifest.model.model_ref != VIDEO_GENERATION_MODEL_REF
    {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: format!(
                "requires builtin model ref {VIDEO_GENERATION_MODEL_REF}, got kind={} ref={}",
                manifest.model.kind.as_wire_str(),
                manifest.model.model_ref
            ),
        });
    }
    let JobType::VideoGeneration {
        width,
        height,
        fps,
        duration_secs,
    } = manifest.job_type
    else {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: "manifest is not a video_generation job".to_string(),
        });
    };
    // Allowlist is enforced on the control plane; the agent re-checks the closed
    // product so a tampered manifest cannot invent geometry.
    let key = format!("{width}x{height}@{fps}fpsx{duration_secs}s");
    const ALLOWED: &[&str] = &[
        "256x256@8fpsx1s",
        "256x256@8fpsx2s",
        "512x512@8fpsx1s",
        "512x512@8fpsx2s",
        "512x512@24fpsx1s",
        "768x768@8fpsx2s",
    ];
    if !ALLOWED.contains(&key.as_str()) {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: format!("profile {key} is not offered"),
        });
    }
    Ok(VideoSpec {
        width,
        height,
        fps,
        duration_secs,
    })
}

fn segment_assignment(manifest: &JobManifest) -> Result<SegmentAssignment, RunError> {
    let Some(segment) = manifest.params.get("media_segment") else {
        return Ok(SegmentAssignment {
            ordinal: 0,
            unit_count: 1,
        });
    };
    let ordinal = segment
        .get("ordinal")
        .and_then(|v| v.as_u64())
        .ok_or_else(|| RunError::BadInput {
            job: "video_generation",
            msg: "media_segment.ordinal must be a non-negative integer".to_string(),
        })?;
    let unit_count = segment
        .get("unit_count")
        .and_then(|v| v.as_u64())
        .ok_or_else(|| RunError::BadInput {
            job: "video_generation",
            msg: "media_segment.unit_count must be a positive integer".to_string(),
        })?;
    if unit_count == 0 || unit_count > 64 {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: format!("media_segment.unit_count {unit_count} is outside [1,64]"),
        });
    }
    if ordinal >= unit_count {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: format!("media_segment.ordinal {ordinal} is outside [0,{unit_count})"),
        });
    }
    Ok(SegmentAssignment {
        ordinal: ordinal as u32,
        unit_count: unit_count as u32,
    })
}

fn segment_extent(duration_secs: f64, assignment: SegmentAssignment) -> (f64, f64) {
    let start = duration_secs * f64::from(assignment.ordinal) / f64::from(assignment.unit_count);
    let mut end =
        duration_secs * f64::from(assignment.ordinal + 1) / f64::from(assignment.unit_count);
    if assignment.ordinal + 1 == assignment.unit_count {
        end = duration_secs;
    }
    (start, end)
}

fn parse_prompt(input: &[u8]) -> Result<VideoPromptDoc, RunError> {
    if input.is_empty() || input.len() > MAX_PROMPT_BYTES {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: format!(
                "prompt document must contain 1..{MAX_PROMPT_BYTES} bytes, got {}",
                input.len()
            ),
        });
    }
    let doc: VideoPromptDoc = serde_json::from_slice(input).map_err(|e| RunError::BadInput {
        job: "video_generation",
        msg: format!("prompt document is not the closed JSON contract: {e}"),
    })?;
    if doc.prompt.trim().is_empty() {
        return Err(RunError::BadInput {
            job: "video_generation",
            msg: "prompt is required".to_string(),
        });
    }
    Ok(doc)
}

/// Deterministic RGB segment. Same (prompt, seed, geometry, ordinal window)
/// under this engine always yields the same bytes. Cross-supplier agreement is
/// not claimed and is refused at the control plane.
fn synthesize_segment(
    spec: VideoSpec,
    doc: &VideoPromptDoc,
    assignment: SegmentAssignment,
) -> Vec<u8> {
    let duration = f64::from(spec.duration_secs);
    let (start_secs, end_secs) = segment_extent(duration, assignment);
    let start_frame = (start_secs * f64::from(spec.fps)).floor() as i64;
    let mut end_frame = (end_secs * f64::from(spec.fps)).floor() as i64;
    if end_frame <= start_frame {
        end_frame = start_frame + 1;
    }
    let frame_count = (end_frame - start_frame) as u32;
    let pixels = (spec.width as usize) * (spec.height as usize) * 3;
    let mut out = Vec::with_capacity(64 + pixels * frame_count as usize);
    out.extend_from_slice(b"MERCVIDEO1\n");
    out.extend_from_slice(
        format!(
            "{} {} {} {} {} {} {}\n",
            spec.width,
            spec.height,
            spec.fps,
            frame_count,
            doc.seed,
            assignment.ordinal,
            assignment.unit_count
        )
        .as_bytes(),
    );
    let base = Sha256::digest(doc.prompt.as_bytes());
    let mut frame = vec![0u8; pixels];
    for f in 0..frame_count {
        let abs = start_frame + i64::from(f);
        for (i, px) in frame.iter_mut().enumerate() {
            *px = (u32::from(base[i % 32])
                .wrapping_add(doc.seed as u32)
                .wrapping_add((abs as u32).wrapping_mul(3))
                .wrapping_add(i as u32)) as u8;
        }
        out.extend_from_slice(&frame);
    }
    out
}

pub struct VideoGenerationRunner;

impl VideoGenerationRunner {
    fn generate(&self, manifest: &JobManifest, input: &[u8]) -> Result<Vec<u8>, RunError> {
        let spec = video_spec(manifest)?;
        let assignment = segment_assignment(manifest)?;
        let doc = parse_prompt(input)?;
        Ok(synthesize_segment(spec, &doc, assignment))
    }

    pub(crate) async fn benchmark(&self) -> Result<BenchResult, RunError> {
        let manifest = benchmark_manifest();
        let input = br#"{"prompt":"benchmark lighthouse","seed":1}"#;
        let started = Instant::now();
        let _out = self.generate(&manifest, input)?;
        let elapsed = started.elapsed().as_secs_f64().max(1e-9);
        let frames = 8.0; // 256x256@8fpsx1s
        Ok(BenchResult {
            model_id: VIDEO_GENERATION_MODEL_REF.to_string(),
            job_type: "video_generation".to_string(),
            tps: 0.0,
            eps: (frames / elapsed) as f32,
            p99_ms: (elapsed * 1000.0) as u32,
            thermal_ok: true,
            load_ms: 0,
        })
    }
}

#[async_trait]
impl JobRunner for VideoGenerationRunner {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool {
        matches!(manifest.job_type, JobType::VideoGeneration { .. })
            && manifest.model.kind == ModelKind::Builtin
            && manifest.model.model_ref == VIDEO_GENERATION_MODEL_REF
            && cap
                .supported_jobs
                .iter()
                .any(|job| job == "video_generation")
            && cap
                .supported_models
                .iter()
                .any(|model| model == VIDEO_GENERATION_MODEL_REF)
            && cap.memory_gb >= manifest.constraints.min_memory_gb
    }

    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        _pool: &ModelPool,
    ) -> Result<JobOutput, RunError> {
        let started = Instant::now();
        let result = self.generate(manifest, input)?;
        Ok(JobOutput {
            result,
            binary: true,
            content_type: "application/x-merc-video",
            duration_ms: started.elapsed().as_millis() as u64,
            tokens_used: 0,
            inference_backend: VIDEO_BACKEND.to_string(),
        })
    }

    fn backend_name(&self) -> &'static str {
        VIDEO_BACKEND
    }
}

fn benchmark_manifest() -> JobManifest {
    JobManifest {
        id: Uuid::nil(),
        job_type: JobType::VideoGeneration {
            width: 256,
            height: 256,
            fps: 8,
            duration_secs: 1,
        },
        model: ModelRef {
            kind: ModelKind::Builtin,
            model_ref: VIDEO_GENERATION_MODEL_REF.into(),
        },
        inputs: vec![InputRef {
            url: "benchmark://prompt".into(),
            bytes: 0,
        }],
        output: OutputRef {
            url: "benchmark://video".into(),
        },
        params: serde_json::Value::Null,
        constraints: JobConstraints {
            min_memory_gb: 1.0,
            hw_classes: None,
            max_duration_secs: 60,
            data_residency: None,
        },
        verification: VerificationPolicy {
            redundancy_frac: 0.0,
            honeypot_frac: 0.0,
            payout_hold_secs: 0,
        },
        tier: ServiceTier::Batch,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn same_segment_twice_is_byte_equal() {
        let runner = VideoGenerationRunner;
        let mut manifest = benchmark_manifest();
        manifest.params = serde_json::json!({
            "media_segment": {"version":"MERC_MEDIA_SEGMENT_PLAN_V1","ordinal":0,"unit_count":2,"ordering":"TIME_ORDINAL_V1"}
        });
        manifest.job_type = JobType::VideoGeneration {
            width: 256,
            height: 256,
            fps: 8,
            duration_secs: 2,
        };
        let input = br#"{"prompt":"a lighthouse at dusk","seed":9}"#;
        let a = runner.generate(&manifest, input).unwrap();
        let b = runner.generate(&manifest, input).unwrap();
        assert_eq!(a, b);
        assert!(a.starts_with(b"MERCVIDEO1\n"));
    }

    #[test]
    fn multi_segment_reassembly_matches_continuous() {
        let runner = VideoGenerationRunner;
        let input = br#"{"prompt":"a lighthouse at dusk","seed":9}"#;
        let mut segs = Vec::new();
        for ordinal in 0..2u32 {
            let mut manifest = benchmark_manifest();
            manifest.job_type = JobType::VideoGeneration {
                width: 256,
                height: 256,
                fps: 8,
                duration_secs: 2,
            };
            manifest.params = serde_json::json!({
                "media_segment": {
                    "version":"MERC_MEDIA_SEGMENT_PLAN_V1",
                    "ordinal": ordinal,
                    "unit_count": 2,
                    "ordering":"TIME_ORDINAL_V1"
                }
            });
            segs.push(runner.generate(&manifest, input).unwrap());
        }
        let continuous = {
            let mut manifest = benchmark_manifest();
            manifest.job_type = JobType::VideoGeneration {
                width: 256,
                height: 256,
                fps: 8,
                duration_secs: 2,
            };
            runner.generate(&manifest, input).unwrap()
        };
        // Strip headers and compare payloads: continuous header has unit_count=1.
        let payload = |b: &[u8]| {
            let rest = &b["MERCVIDEO1\n".len()..];
            let nl = rest.iter().position(|&c| c == b'\n').unwrap();
            rest[nl + 1..].to_vec()
        };
        let mut merged = Vec::new();
        for s in &segs {
            merged.extend_from_slice(&payload(s));
        }
        assert_eq!(merged, payload(&continuous));
    }
}
