use crate::executor::RunError;
use crate::hardware::MemorySnapshot;
use crate::types::{FailMemory, FailReport};

pub fn classify(err: &RunError, low_memory: bool) -> &'static str {
    match err {
        RunError::NoRunner { .. } => "unsupported_job_type",
        RunError::ModelFetch { .. } => "model_load_failed",
        RunError::BadInput { .. } => "bad_input",
        RunError::Inference { msg, .. } => {
            let m = msg.to_ascii_lowercase();
            if m.contains("input_url") || m.contains("output_url") || m.contains("presigned") {
                "object_store_error"
            } else if low_memory || m.contains("out of memory") || m.contains("oom") {
                "oom"
            } else {
                "internal_error"
            }
        }
        RunError::OomPreempt { .. } => "oom",
    }
}

pub fn build_report(
    err: &RunError,
    backend: &str,
    model: &str,
    duration_ms: u64,
    snap: &MemorySnapshot,
    headroom_gb: f32,
) -> FailReport {
    let effective = (snap.available_gb - headroom_gb).max(0.0);
    let low = effective <= 0.5 || snap.available_gb <= headroom_gb;
    FailReport {
        class: classify(err, low).to_string(),
        message: err.to_string(),
        duration_ms,
        backend: backend.to_string(),
        model: model.to_string(),
        memory: Some(FailMemory {
            total_gb: snap.total_gb,
            available_gb: snap.available_gb,
            effective_gb: effective,
            reserved_headroom_gb: headroom_gb,
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_run_errors_to_shared_taxonomy() {
        assert_eq!(
            classify(
                &RunError::BadInput {
                    job: "embed",
                    msg: "x".into()
                },
                false
            ),
            "bad_input"
        );
        assert_eq!(
            classify(
                &RunError::ModelFetch {
                    repo: "r".into(),
                    msg: "x".into()
                },
                false
            ),
            "model_load_failed"
        );
        assert_eq!(
            classify(
                &RunError::Inference {
                    backend: "embed",
                    msg: "fetching input_url: timeout".into()
                },
                false
            ),
            "object_store_error"
        );
        assert_eq!(
            classify(
                &RunError::Inference {
                    backend: "batch_infer",
                    msg: "metal alloc failed".into()
                },
                true
            ),
            "oom"
        );
        assert_eq!(
            classify(
                &RunError::Inference {
                    backend: "batch_infer",
                    msg: "tokenizer error".into()
                },
                false
            ),
            "internal_error"
        );
    }

    #[test]
    fn build_report_carries_real_memory_and_oom_signal() {
        let snap = MemorySnapshot {
            total_gb: 64.0,
            available_gb: 1.0,
        };
        let err = RunError::Inference {
            backend: "batch_infer",
            msg: "alloc".into(),
        };
        let r = build_report(
            &err,
            "batch_infer",
            "llama-3.2-1b-instruct-q4",
            1200,
            &snap,
            8.0,
        );
        assert_eq!(r.class, "oom");
        let m = r.memory.unwrap();
        assert_eq!(m.total_gb, 64.0);
        assert_eq!(m.effective_gb, 0.0); // max(1 - 8, 0)
        assert_eq!(m.reserved_headroom_gb, 8.0);
    }
}
