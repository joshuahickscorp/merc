use std::path::{Path, PathBuf};
use std::process::Stdio;

use async_trait::async_trait;
use serde::Deserialize;
use tokio::process::Command;
use uuid::Uuid;

use crate::executor::{JobOutput, JobRunner, RunError};
use crate::pool::ModelPool;
use crate::types::{JobManifest, JobType, ModelKind, WorkerCapability};

// This model ref is an executable identity, not a model-card alias. The public
// control plane does not advertise it yet; accepting a near match here would
// turn a future runtime-matrix typo into arbitrary local process execution.
pub const MEDIA_TRANSCODE_MODEL_REF: &str = "ffmpeg-transcode-v1";

const MAX_MEDIA_INPUT_BYTES: usize = 512 << 20;
const MAX_MEDIA_OUTPUT_BYTES: u64 = 512 << 20;
const MEDIA_BACKEND: &str = "ffmpeg_transcode";

#[derive(Debug, Clone, Copy)]
struct MediaTranscodeSpec {
    input_demuxer: &'static str,
    input_extension: &'static str,
    max_width: u32,
    max_height: u32,
    fps: u32,
    video_bitrate_kbps: u32,
}

fn media_transcode_spec(manifest: &JobManifest) -> Result<MediaTranscodeSpec, RunError> {
    if manifest.model.kind != ModelKind::Builtin
        || manifest.model.model_ref != MEDIA_TRANSCODE_MODEL_REF
    {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!(
                "requires builtin model ref {MEDIA_TRANSCODE_MODEL_REF}, got kind={} ref={}",
                manifest.model.kind.as_wire_str(),
                manifest.model.model_ref
            ),
        });
    }
    let JobType::MediaTranscode {
        input_format,
        max_width,
        max_height,
        fps,
        video_bitrate_kbps,
    } = &manifest.job_type
    else {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: "manifest is not a media_transcode job".to_string(),
        });
    };
    let (input_demuxer, input_extension) = match input_format.as_str() {
        "mp4" | "mov" => ("mov", "mp4"),
        "webm" => ("webm", "webm"),
        "mkv" => ("matroska", "mkv"),
        _ => {
            return Err(RunError::BadInput {
                job: "media_transcode",
                msg: format!("input_format {input_format:?} is not one of mp4, mov, webm, mkv"),
            })
        }
    };
    for (name, value) in [("max_width", *max_width), ("max_height", *max_height)] {
        if !(64..=4096).contains(&value) || value % 2 != 0 {
            return Err(RunError::BadInput {
                job: "media_transcode",
                msg: format!("{name} must be an even value in [64,4096], got {value}"),
            });
        }
    }
    if !(1..=60).contains(fps) {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("fps must be in [1,60], got {fps}"),
        });
    }
    if !(200..=50_000).contains(video_bitrate_kbps) {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("video_bitrate_kbps must be in [200,50000], got {video_bitrate_kbps}"),
        });
    }
    Ok(MediaTranscodeSpec {
        input_demuxer,
        input_extension,
        max_width: *max_width,
        max_height: *max_height,
        fps: *fps,
        video_bitrate_kbps: *video_bitrate_kbps,
    })
}

// MediaTranscodeRunner is intentionally narrow. It is an actual FFmpeg process
// runner, but it is not a buyer-facing capability until the control plane has
// a governed cell, an independent verifier, and settlement authority for the
// media workload. Keeping it unadvertised prevents this implementation work
// from being mistaken for a sellable lane.
pub struct MediaTranscodeRunner {
    ffmpeg: PathBuf,
    ffprobe: PathBuf,
}

impl MediaTranscodeRunner {
    pub fn from_environment() -> Self {
        Self::new(
            executable_from_env("MERC_FFMPEG_PATH", "ffmpeg"),
            executable_from_env("MERC_FFPROBE_PATH", "ffprobe"),
        )
    }

    pub fn new(ffmpeg: PathBuf, ffprobe: PathBuf) -> Self {
        Self { ffmpeg, ffprobe }
    }

    async fn transcode(&self, spec: MediaTranscodeSpec, input: &[u8]) -> Result<Vec<u8>, RunError> {
        if input.is_empty() || input.len() > MAX_MEDIA_INPUT_BYTES {
            return Err(RunError::BadInput {
                job: "media_transcode",
                msg: format!(
                    "input must contain 1..{MAX_MEDIA_INPUT_BYTES} bytes, got {}",
                    input.len()
                ),
            });
        }
        let scratch = MediaScratch::new().map_err(media_err)?;
        let source = scratch
            .path()
            .join(format!("source.{}", spec.input_extension));
        let output = scratch.path().join("output.mp4");
        tokio::fs::write(&source, input).await.map_err(media_err)?;

        // Every variable below is a validated numeric value or a path generated
        // inside this scratch directory. There is no shell and no buyer supplied
        // argument position, URL, protocol, codec, filter, or output path.
        let scale = format!(
            "scale={}:{}:force_original_aspect_ratio=decrease,pad={}:{}:(ow-iw)/2:(oh-ih):black",
            spec.max_width, spec.max_height, spec.max_width, spec.max_height
        );
        let bitrate = format!("{}k", spec.video_bitrate_kbps);
        let buffer = format!("{}k", spec.video_bitrate_kbps.saturating_mul(2));
        let fps = spec.fps.to_string();
        let run = Command::new(&self.ffmpeg)
            .kill_on_drop(true)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .args([
                "-nostdin",
                "-hide_banner",
                "-loglevel",
                "error",
                "-xerror",
                "-fflags",
                "+bitexact",
                "-f",
                spec.input_demuxer,
                "-i",
            ])
            .arg(&source)
            .args([
                "-map_metadata",
                "-1",
                "-map_chapters",
                "-1",
                "-map",
                "0:v:0",
                "-an",
                "-vf",
            ])
            .arg(&scale)
            .args(["-r", &fps, "-c:v", "libx264", "-preset", "medium", "-b:v"])
            .arg(&bitrate)
            .args([
                "-maxrate",
                &bitrate,
                "-bufsize",
                &buffer,
                "-pix_fmt",
                "yuv420p",
                "-threads",
                "1",
                "-flags:v",
                "+bitexact",
                "-movflags",
                "+faststart",
                "-f",
                "mp4",
            ])
            .arg(&output)
            .output()
            .await
            .map_err(media_err)?;
        if !run.status.success() {
            return Err(RunError::Inference {
                backend: MEDIA_BACKEND,
                msg: format!(
                    "ffmpeg exited {}: {}",
                    run.status,
                    bounded_process_error(&run.stderr)
                ),
            });
        }
        verify_media_output(&self.ffprobe, &output, spec).await?;
        let metadata = tokio::fs::metadata(&output).await.map_err(media_err)?;
        if metadata.len() == 0 || metadata.len() > MAX_MEDIA_OUTPUT_BYTES {
            return Err(RunError::Inference {
                backend: MEDIA_BACKEND,
                msg: format!(
                    "ffmpeg output has {} bytes; expected 1..{MAX_MEDIA_OUTPUT_BYTES}",
                    metadata.len()
                ),
            });
        }
        tokio::fs::read(output).await.map_err(media_err)
    }
}

#[async_trait]
impl JobRunner for MediaTranscodeRunner {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool {
        matches!(manifest.job_type, JobType::MediaTranscode { .. })
            && manifest.model.kind == ModelKind::Builtin
            && manifest.model.model_ref == MEDIA_TRANSCODE_MODEL_REF
            && cap
                .supported_jobs
                .iter()
                .any(|job| job == "media_transcode")
            && cap
                .supported_models
                .iter()
                .any(|model| model == MEDIA_TRANSCODE_MODEL_REF)
            && cap.memory_gb >= manifest.constraints.min_memory_gb
    }

    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        _pool: &ModelPool,
    ) -> Result<JobOutput, RunError> {
        let started = std::time::Instant::now();
        let spec = media_transcode_spec(manifest)?;
        let result = self.transcode(spec, input).await?;
        Ok(JobOutput {
            result,
            binary: true,
            content_type: "video/mp4",
            duration_ms: started.elapsed().as_millis() as u64,
            tokens_used: 0,
            inference_backend: MEDIA_BACKEND.to_string(),
        })
    }

    fn backend_name(&self) -> &'static str {
        MEDIA_BACKEND
    }
}

fn executable_from_env(name: &str, default: &str) -> PathBuf {
    std::env::var_os(name)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(default))
}

fn media_err(error: impl std::fmt::Display) -> RunError {
    RunError::Inference {
        backend: MEDIA_BACKEND,
        msg: error.to_string(),
    }
}

fn bounded_process_error(stderr: &[u8]) -> String {
    const MAX: usize = 1024;
    let rendered = String::from_utf8_lossy(stderr);
    let rendered = rendered.trim();
    if rendered.len() <= MAX {
        rendered.to_string()
    } else {
        format!("{}…", &rendered[..MAX])
    }
}

#[derive(Deserialize)]
struct ProbeOutput {
    streams: Vec<ProbeStream>,
}

#[derive(Deserialize)]
struct ProbeStream {
    codec_type: String,
    width: Option<u32>,
    height: Option<u32>,
}

async fn verify_media_output(
    ffprobe: &Path,
    output: &Path,
    spec: MediaTranscodeSpec,
) -> Result<(), RunError> {
    let probe = Command::new(ffprobe)
        .kill_on_drop(true)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .args([
            "-v",
            "error",
            "-show_entries",
            "stream=codec_type,width,height",
            "-of",
            "json",
        ])
        .arg(output)
        .output()
        .await
        .map_err(media_err)?;
    if !probe.status.success() {
        return Err(RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: format!(
                "ffprobe exited {}: {}",
                probe.status,
                bounded_process_error(&probe.stderr)
            ),
        });
    }
    let parsed: ProbeOutput = serde_json::from_slice(&probe.stdout).map_err(media_err)?;
    let video = parsed
        .streams
        .iter()
        .find(|stream| stream.codec_type == "video")
        .ok_or_else(|| RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: "ffprobe found no video stream in FFmpeg output".to_string(),
        })?;
    let (width, height) = match (video.width, video.height) {
        (Some(width), Some(height)) if width > 0 && height > 0 => (width, height),
        _ => {
            return Err(RunError::Inference {
                backend: MEDIA_BACKEND,
                msg: "ffprobe reported a video stream without positive dimensions".to_string(),
            })
        }
    };
    if width > spec.max_width || height > spec.max_height || width % 2 != 0 || height % 2 != 0 {
        return Err(RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: format!(
                "output geometry {width}x{height} does not satisfy bounded even geometry {}x{}",
                spec.max_width, spec.max_height
            ),
        });
    }
    Ok(())
}

// The runner writes sensitive buyer media only beneath a unique local scratch
// path and removes it on every normal, error, cancellation, or deadline-drop
// path. kill_on_drop above ensures FFmpeg/ffprobe do not retain an open file
// descriptor after the task future is cancelled.
struct MediaScratch {
    path: PathBuf,
}

impl MediaScratch {
    fn new() -> std::io::Result<Self> {
        for _ in 0..8 {
            let path = std::env::temp_dir().join(format!("merc-media-{}", Uuid::new_v4()));
            match std::fs::create_dir(&path) {
                Ok(()) => return Ok(Self { path }),
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(error) => return Err(error),
            }
        }
        Err(std::io::Error::new(
            std::io::ErrorKind::AlreadyExists,
            "could not allocate unique media scratch directory",
        ))
    }

    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for MediaScratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{
        HardwareClass, InputRef, JobConstraints, ModelRef, OutputRef, ServiceTier,
        VerificationPolicy,
    };

    fn manifest() -> JobManifest {
        JobManifest {
            id: Uuid::nil(),
            job_type: JobType::MediaTranscode {
                input_format: "mp4".to_string(),
                max_width: 320,
                max_height: 180,
                fps: 24,
                video_bitrate_kbps: 400,
            },
            model: ModelRef {
                kind: ModelKind::Builtin,
                model_ref: MEDIA_TRANSCODE_MODEL_REF.to_string(),
            },
            inputs: vec![InputRef {
                url: String::new(),
                bytes: 0,
            }],
            output: OutputRef { url: String::new() },
            params: serde_json::Value::Null,
            constraints: JobConstraints {
                min_memory_gb: 1.0,
                hw_classes: None,
                max_duration_secs: 60,
                data_residency: None,
            },
            verification: VerificationPolicy {
                redundancy_frac: 1.0,
                honeypot_frac: 0.0,
                payout_hold_secs: 0,
            },
            tier: ServiceTier::Batch,
        }
    }

    fn media_capability() -> WorkerCapability {
        WorkerCapability {
            worker_id: Uuid::nil(),
            supplier_id: Uuid::nil(),
            agent_session_id: Uuid::nil(),
            hw_class: HardwareClass::Cpu,
            engine: "ffmpeg".to_string(),
            build_hash: String::new(),
            memory_gb: 8.0,
            memory_bw_gbps: 0.0,
            gpu_count: 0,
            memory_gb_per_gpu: 0.0,
            interconnect: String::new(),
            supported_jobs: vec!["media_transcode".to_string()],
            supported_models: vec![MEDIA_TRANSCODE_MODEL_REF.to_string()],
            benchmarks: vec![],
            agent_version: String::new(),
            os_version: String::new(),
            min_payout_usd_hr: 0.0,
        }
    }

    #[test]
    fn media_transcode_rejects_unbounded_or_non_builtin_requests() {
        let mut job = manifest();
        match &mut job.job_type {
            JobType::MediaTranscode { max_width, .. } => *max_width = 321,
            _ => unreachable!(),
        }
        assert!(matches!(
            media_transcode_spec(&job),
            Err(RunError::BadInput { .. })
        ));
        job = manifest();
        job.model.kind = ModelKind::Hf;
        assert!(matches!(
            media_transcode_spec(&job),
            Err(RunError::BadInput { .. })
        ));
    }

    #[tokio::test]
    async fn media_transcode_runs_real_ffmpeg_when_present() {
        let runner = MediaTranscodeRunner::from_environment();
        if Command::new(&runner.ffmpeg)
            .arg("-version")
            .output()
            .await
            .is_err()
            || Command::new(&runner.ffprobe)
                .arg("-version")
                .output()
                .await
                .is_err()
        {
            eprintln!("skipping real media transcode: ffmpeg/ffprobe are not installed");
            return;
        }
        let scratch = MediaScratch::new().expect("scratch");
        let source = scratch.path().join("source.mp4");
        let generated = Command::new(&runner.ffmpeg)
            .kill_on_drop(true)
            .args([
                "-nostdin",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                "color=c=blue:s=160x90:r=24",
                "-t",
                "0.2",
                "-pix_fmt",
                "yuv420p",
                "-f",
                "mp4",
            ])
            .arg(&source)
            .output()
            .await
            .expect("generate source media");
        assert!(
            generated.status.success(),
            "{}",
            bounded_process_error(&generated.stderr)
        );
        let input = tokio::fs::read(source).await.expect("source bytes");
        let output = runner
            .run(&manifest(), &input, &ModelPool::new())
            .await
            .expect("real ffmpeg transcode");
        assert!(output.binary);
        assert_eq!(output.content_type, "video/mp4");
        assert!(output.result.len() > 100);
        assert_eq!(output.tokens_used, 0);
        assert!(runner.can_run(&manifest(), &media_capability()).await);
    }
}
