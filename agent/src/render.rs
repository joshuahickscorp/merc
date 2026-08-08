use std::time::Instant;

use async_trait::async_trait;
use serde::Deserialize;
use uuid::Uuid;

use crate::executor::{JobOutput, JobRunner, RunError};
use crate::pool::ModelPool;
use crate::types::{
    BenchResult, InputRef, JobConstraints, JobManifest, JobType, ModelKind, ModelRef, OutputRef,
    ServiceTier, VerificationPolicy, WorkerCapability,
};

pub const MEDIA_RENDERING_MODEL_REF: &str = "svg-scene-render-v1";
const MAX_SCENE_BYTES: usize = 8 << 20;
const MAX_PIXELS: u64 = 1024 * 1024;
const MAX_PRIMITIVES: usize = 256;
const RENDER_BACKEND: &str = "builtin_scene_rasterizer";

#[derive(Debug, Clone, Copy)]
struct RenderSpec {
    width: u32,
    height: u32,
}

#[derive(Debug, Deserialize)]
struct Scene {
    background: [u8; 3],
    #[serde(default)]
    rectangles: Vec<Rectangle>,
}

#[derive(Debug, Deserialize)]
struct Rectangle {
    x: u32,
    y: u32,
    width: u32,
    height: u32,
    color: [u8; 3],
}

fn render_spec(manifest: &JobManifest) -> Result<RenderSpec, RunError> {
    if manifest.model.kind != ModelKind::Builtin
        || manifest.model.model_ref != MEDIA_RENDERING_MODEL_REF
    {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: format!(
                "requires builtin model ref {MEDIA_RENDERING_MODEL_REF}, got kind={} ref={}",
                manifest.model.kind.as_wire_str(),
                manifest.model.model_ref
            ),
        });
    }
    let JobType::MediaRendering { width, height } = manifest.job_type else {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: "manifest is not a media_rendering job".to_string(),
        });
    };
    if !(16..=1024).contains(&width) || !(16..=1024).contains(&height) {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: format!("width and height must be in [16,1024], got {width}x{height}"),
        });
    }
    if u64::from(width) * u64::from(height) > MAX_PIXELS {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: "render canvas exceeds one-million-pixel bound".to_string(),
        });
    }
    Ok(RenderSpec { width, height })
}

fn parse_scene(input: &[u8], spec: RenderSpec) -> Result<Scene, RunError> {
    if input.is_empty() || input.len() > MAX_SCENE_BYTES {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: format!(
                "scene must contain 1..{MAX_SCENE_BYTES} bytes, got {}",
                input.len()
            ),
        });
    }
    let scene: Scene = serde_json::from_slice(input).map_err(|e| RunError::BadInput {
        job: "media_rendering",
        msg: format!("scene is not the closed JSON rendering contract: {e}"),
    })?;
    if scene.rectangles.len() > MAX_PRIMITIVES {
        return Err(RunError::BadInput {
            job: "media_rendering",
            msg: format!(
                "scene has {} rectangles; maximum is {MAX_PRIMITIVES}",
                scene.rectangles.len()
            ),
        });
    }
    for (i, rect) in scene.rectangles.iter().enumerate() {
        if rect.width == 0 || rect.height == 0 {
            return Err(RunError::BadInput {
                job: "media_rendering",
                msg: format!("rectangle {i} has zero area"),
            });
        }
        let right = u64::from(rect.x) + u64::from(rect.width);
        let bottom = u64::from(rect.y) + u64::from(rect.height);
        if right > u64::from(spec.width) || bottom > u64::from(spec.height) {
            return Err(RunError::BadInput {
                job: "media_rendering",
                msg: format!("rectangle {i} lies outside the declared canvas"),
            });
        }
    }
    Ok(scene)
}

fn rasterize(spec: RenderSpec, scene: &Scene) -> Vec<u8> {
    let pixels = usize::try_from(u64::from(spec.width) * u64::from(spec.height) * 3)
        .expect("render bound fits usize");
    let mut out = Vec::with_capacity(32 + pixels);
    out.extend_from_slice(format!("P6\n{} {}\n255\n", spec.width, spec.height).as_bytes());
    out.resize(out.len() + pixels, 0);
    let pixel_start = out.len() - pixels;
    for pixel in out[pixel_start..].chunks_exact_mut(3) {
        pixel.copy_from_slice(&scene.background);
    }
    for rect in &scene.rectangles {
        for y in rect.y..rect.y + rect.height {
            for x in rect.x..rect.x + rect.width {
                let offset = ((y * spec.width + x) * 3) as usize;
                let start = pixel_start + offset;
                out[start..start + 3].copy_from_slice(&rect.color);
            }
        }
    }
    out
}

pub struct MediaRenderingRunner;

impl MediaRenderingRunner {
    fn render(&self, manifest: &JobManifest, input: &[u8]) -> Result<Vec<u8>, RunError> {
        let spec = render_spec(manifest)?;
        let scene = parse_scene(input, spec)?;
        Ok(rasterize(spec, &scene))
    }

    pub(crate) async fn benchmark(&self) -> Result<BenchResult, RunError> {
        let manifest = benchmark_manifest();
        let input = br#"{"background":[8,16,32],"rectangles":[{"x":4,"y":4,"width":24,"height":24,"color":[220,80,40]}]}"#;
        let mut slowest = 0.0_f64;
        for _ in 0..32 {
            let started = Instant::now();
            let output = self.render(&manifest, input)?;
            if output.is_empty() {
                return Err(RunError::Inference {
                    backend: RENDER_BACKEND,
                    msg: "empty render".into(),
                });
            }
            slowest = slowest.max(started.elapsed().as_secs_f64());
        }
        let pixels = 64.0 * 64.0;
        Ok(BenchResult {
            model_id: MEDIA_RENDERING_MODEL_REF.to_string(),
            job_type: "media_rendering".to_string(),
            tps: (pixels / slowest.max(1e-6)) as f32,
            eps: 0.0,
            p99_ms: (slowest * 1000.0).ceil() as u32,
            thermal_ok: true,
            load_ms: 0,
        })
    }
}

#[async_trait]
impl JobRunner for MediaRenderingRunner {
    async fn can_run(&self, manifest: &JobManifest, cap: &WorkerCapability) -> bool {
        matches!(manifest.job_type, JobType::MediaRendering { .. })
            && manifest.model.kind == ModelKind::Builtin
            && manifest.model.model_ref == MEDIA_RENDERING_MODEL_REF
            && cap
                .supported_jobs
                .iter()
                .any(|job| job == "media_rendering")
            && cap
                .supported_models
                .iter()
                .any(|model| model == MEDIA_RENDERING_MODEL_REF)
            && cap.memory_gb >= manifest.constraints.min_memory_gb
    }

    async fn run(
        &self,
        manifest: &JobManifest,
        input: &[u8],
        _pool: &ModelPool,
    ) -> Result<JobOutput, RunError> {
        let started = Instant::now();
        let result = self.render(manifest, input)?;
        Ok(JobOutput {
            result,
            binary: true,
            content_type: "image/x-portable-pixmap",
            duration_ms: started.elapsed().as_millis() as u64,
            tokens_used: 0,
            inference_backend: RENDER_BACKEND.to_string(),
            // Rendering has no prompt cache to observe.
            cached_prompt_tokens: None,
        })
    }

    fn backend_name(&self) -> &'static str {
        RENDER_BACKEND
    }
}

fn benchmark_manifest() -> JobManifest {
    JobManifest {
        id: Uuid::nil(),
        job_type: JobType::MediaRendering {
            width: 64,
            height: 64,
        },
        model: ModelRef {
            kind: ModelKind::Builtin,
            model_ref: MEDIA_RENDERING_MODEL_REF.into(),
        },
        inputs: vec![InputRef {
            url: "benchmark://scene".into(),
            bytes: 0,
        }],
        output: OutputRef {
            url: "benchmark://render".into(),
        },
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn render_is_deterministic_and_bounded() {
        let runner = MediaRenderingRunner;
        let manifest = benchmark_manifest();
        let input = br#"{"background":[0,0,0],"rectangles":[{"x":1,"y":1,"width":2,"height":2,"color":[255,0,0]}]}"#;
        let a = runner.render(&manifest, input).unwrap();
        let b = runner.render(&manifest, input).unwrap();
        assert_eq!(a, b);
        assert!(a.starts_with(b"P6\n64 64\n255\n"));
        assert_eq!(a.len(), "P6\n64 64\n255\n".len() + 64 * 64 * 3);
    }

    #[test]
    fn render_rejects_out_of_canvas_scene() {
        let runner = MediaRenderingRunner;
        let manifest = benchmark_manifest();
        let input = br#"{"background":[0,0,0],"rectangles":[{"x":63,"y":63,"width":2,"height":2,"color":[255,0,0]}]}"#;
        assert!(matches!(
            runner.render(&manifest, input),
            Err(RunError::BadInput { .. })
        ));
    }
}
