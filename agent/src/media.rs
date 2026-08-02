use std::path::{Path, PathBuf};
use std::process::{Output, Stdio};
use std::time::Duration;

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
const MAX_MEDIA_INPUT_WIDTH: u32 = 4096;
const MAX_MEDIA_INPUT_HEIGHT: u32 = 4096;
const MAX_MEDIA_INPUT_DURATION_SECS: f64 = 120.0;
const MAX_MEDIA_INPUT_FPS: f64 = 60.0;
const MEDIA_PROCESS_TIMEOUT: Duration = Duration::from_secs(150);
const MEDIA_PROBE_TIMEOUT: Duration = Duration::from_secs(10);
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
        verify_media_input(&self.ffprobe, &source, spec).await?;

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
        let mut command = Command::new(&self.ffmpeg);
        command
            .kill_on_drop(true)
            // Do not inherit a supplier's process environment. FFmpeg needs no
            // proxy, plugin, home-directory, locale, or user configuration to
            // perform this fixed local-file transcode.
            .env_clear()
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .args([
                "-nostdin",
                "-hide_banner",
                "-loglevel",
                "error",
                "-xerror",
                // This process is only allowed to read the scratch input and
                // write the scratch output. A crafted media container must not
                // turn a parser into a network client.
                "-protocol_whitelist",
                "file,pipe",
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
            .arg(&output);
        let run = bounded_media_command(command, MEDIA_PROCESS_TIMEOUT, "ffmpeg transcode").await?;
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
    let requested = std::env::var_os(name)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(default));
    resolve_executable_before_environment_clear(requested)
}

// Command lookup ordinarily happens in the child, using its PATH. The media
// child intentionally has no inherited environment, so resolve a bare command
// against the agent's own startup PATH before spawning it. An absolute or
// relative explicit path remains explicit; a missing binary is deliberately
// left missing and fails closed when execution starts.
fn resolve_executable_before_environment_clear(requested: PathBuf) -> PathBuf {
    if requested.components().count() != 1 {
        return requested;
    }
    let Some(path) = std::env::var_os("PATH") else {
        return requested;
    };
    for directory in std::env::split_paths(&path) {
        let candidate = directory.join(&requested);
        if candidate.is_file() {
            return candidate;
        }
    }
    requested
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
    #[serde(default)]
    streams: Vec<ProbeStream>,
    #[serde(default)]
    format: Option<ProbeFormat>,
}

#[derive(Deserialize)]
struct ProbeStream {
    codec_type: String,
    width: Option<u32>,
    height: Option<u32>,
    #[serde(default)]
    codec_name: Option<String>,
    #[serde(default)]
    pix_fmt: Option<String>,
    #[serde(default)]
    avg_frame_rate: Option<String>,
}

#[derive(Deserialize)]
struct ProbeFormat {
    #[serde(default)]
    duration: Option<String>,
}

async fn bounded_media_command(
    mut command: Command,
    timeout: Duration,
    operation: &str,
) -> Result<Output, RunError> {
    let child = command.spawn().map_err(media_err)?;
    match tokio::time::timeout(timeout, child.wait_with_output()).await {
        Ok(result) => result.map_err(media_err),
        // `kill_on_drop(true)` is set on every child above. Dropping the
        // wait future here kills and reaps the subprocess instead of leaving a
        // transcoder reading buyer media after its task has failed.
        Err(_) => Err(RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: format!(
                "{operation} exceeded its {}s wall-clock limit",
                timeout.as_secs()
            ),
        }),
    }
}

async fn probe_media_file(ffprobe: &Path, path: &Path) -> Result<ProbeOutput, RunError> {
    let mut command = Command::new(ffprobe);
    command
        .kill_on_drop(true)
        .env_clear()
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .args([
            "-v",
            "error",
            "-show_entries",
            "format=duration:stream=codec_type,codec_name,pix_fmt,width,height,avg_frame_rate",
            "-of",
            "json",
        ])
        .arg(path);
    let probe = bounded_media_command(command, MEDIA_PROBE_TIMEOUT, "ffprobe").await?;
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
    serde_json::from_slice(&probe.stdout).map_err(media_err)
}

fn exactly_one_video_stream<'a>(
    parsed: &'a ProbeOutput,
    purpose: &str,
) -> Result<&'a ProbeStream, RunError> {
    let videos: Vec<_> = parsed
        .streams
        .iter()
        .filter(|stream| stream.codec_type == "video")
        .collect();
    match videos.as_slice() {
        [video] => Ok(*video),
        [] => Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("{purpose} has no video stream"),
        }),
        _ => Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("{purpose} has more than one video stream"),
        }),
    }
}

fn finite_positive_probe_number(value: Option<&str>, field: &str) -> Result<f64, RunError> {
    let rendered = value.unwrap_or("");
    let parsed = rendered.parse::<f64>().map_err(|_| RunError::BadInput {
        job: "media_transcode",
        msg: format!("ffprobe did not report a finite positive {field}"),
    })?;
    if !parsed.is_finite() || parsed <= 0.0 {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("ffprobe did not report a finite positive {field}"),
        });
    }
    Ok(parsed)
}

fn probe_frame_rate(value: Option<&str>, field: &str) -> Result<f64, RunError> {
    let rendered = value.unwrap_or("");
    let (numerator, denominator) = rendered.split_once('/').ok_or_else(|| RunError::BadInput {
        job: "media_transcode",
        msg: format!("ffprobe did not report a finite positive {field}"),
    })?;
    let numerator = numerator.parse::<f64>().map_err(|_| RunError::BadInput {
        job: "media_transcode",
        msg: format!("ffprobe did not report a finite positive {field}"),
    })?;
    let denominator = denominator.parse::<f64>().map_err(|_| RunError::BadInput {
        job: "media_transcode",
        msg: format!("ffprobe did not report a finite positive {field}"),
    })?;
    let fps = numerator / denominator;
    if !numerator.is_finite()
        || !denominator.is_finite()
        || denominator <= 0.0
        || !fps.is_finite()
        || fps <= 0.0
    {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("ffprobe did not report a finite positive {field}"),
        });
    }
    Ok(fps)
}

async fn verify_media_input(
    ffprobe: &Path,
    input: &Path,
    _spec: MediaTranscodeSpec,
) -> Result<(), RunError> {
    let parsed = probe_media_file(ffprobe, input).await?;
    let video = exactly_one_video_stream(&parsed, "input")?;
    let (width, height) = match (video.width, video.height) {
        (Some(width), Some(height)) if width > 0 && height > 0 => (width, height),
        _ => {
            return Err(RunError::BadInput {
                job: "media_transcode",
                msg: "ffprobe reported an input video without positive dimensions".to_string(),
            })
        }
    };
    if width > MAX_MEDIA_INPUT_WIDTH || height > MAX_MEDIA_INPUT_HEIGHT {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!(
                "input geometry {width}x{height} exceeds {MAX_MEDIA_INPUT_WIDTH}x{MAX_MEDIA_INPUT_HEIGHT}"
            ),
        });
    }
    let duration = finite_positive_probe_number(
        parsed
            .format
            .as_ref()
            .and_then(|format| format.duration.as_deref()),
        "input duration",
    )?;
    if duration > MAX_MEDIA_INPUT_DURATION_SECS {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!(
                "input duration {duration:.3}s exceeds {MAX_MEDIA_INPUT_DURATION_SECS:.0}s"
            ),
        });
    }
    let fps = probe_frame_rate(video.avg_frame_rate.as_deref(), "input frame rate")?;
    if fps > MAX_MEDIA_INPUT_FPS {
        return Err(RunError::BadInput {
            job: "media_transcode",
            msg: format!("input frame rate {fps:.3} exceeds {MAX_MEDIA_INPUT_FPS:.0} fps"),
        });
    }
    Ok(())
}

async fn verify_media_output(
    ffprobe: &Path,
    output: &Path,
    spec: MediaTranscodeSpec,
) -> Result<(), RunError> {
    let parsed = probe_media_file(ffprobe, output).await?;
    let video = exactly_one_video_stream(&parsed, "FFmpeg output")?;
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
    if video.codec_name.as_deref() != Some("h264") || video.pix_fmt.as_deref() != Some("yuv420p") {
        return Err(RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: "FFmpeg output is not H.264 yuv420p as the fixed contract requires".to_string(),
        });
    }
    let fps = probe_frame_rate(video.avg_frame_rate.as_deref(), "output frame rate")?;
    if (fps - f64::from(spec.fps)).abs() > 0.01 {
        return Err(RunError::Inference {
            backend: MEDIA_BACKEND,
            msg: format!(
                "output frame rate {fps:.3} does not match requested {} fps",
                spec.fps
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
                Ok(()) => {
                    // Buyer media is written below this path before FFmpeg has
                    // had a chance to validate it. A process-wide umask is not
                    // a confidentiality boundary, so make the directory
                    // private explicitly rather than hoping every launcher's
                    // umask happened to be restrictive.
                    #[cfg(unix)]
                    {
                        use std::os::unix::fs::PermissionsExt;
                        let mut permissions = std::fs::metadata(&path)?.permissions();
                        permissions.set_mode(0o700);
                        std::fs::set_permissions(&path, permissions)?;
                    }
                    return Ok(Self { path });
                }
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

    #[test]
    fn media_probe_numbers_reject_non_finite_and_unbounded_frame_rates() {
        assert_eq!(
            probe_frame_rate(Some("30000/1001"), "test frame rate").expect("fractional rate"),
            30000.0 / 1001.0
        );
        for bad in [None, Some("0/0"), Some("60/0"), Some("nan/1"), Some("60")] {
            assert!(probe_frame_rate(bad, "test frame rate").is_err(), "{bad:?}");
        }
        assert!(finite_positive_probe_number(Some("NaN"), "duration").is_err());
        assert!(finite_positive_probe_number(Some("-1"), "duration").is_err());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn bounded_media_command_kills_a_stuck_subprocess() {
        let mut command = Command::new("/bin/sh");
        command
            .kill_on_drop(true)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .args(["-c", "exec sleep 60"]);
        let err = bounded_media_command(command, Duration::from_millis(20), "test process")
            .await
            .expect_err("stuck child must time out");
        assert!(matches!(err, RunError::Inference { .. }));
        assert!(err.to_string().contains("wall-clock limit"));
    }

    #[cfg(unix)]
    #[test]
    fn media_scratch_directory_is_not_group_or_world_accessible() {
        use std::os::unix::fs::PermissionsExt;

        let scratch = MediaScratch::new().expect("scratch directory");
        let mode = std::fs::metadata(scratch.path())
            .expect("scratch metadata")
            .permissions()
            .mode();
        assert_eq!(
            mode & 0o077,
            0,
            "scratch mode {:o} exposes buyer media",
            mode
        );
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
