use std::future::Future;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

#[derive(Debug, Clone, thiserror::Error)]
pub enum DeadlineError {
    #[error("task has no finite execution deadline")]
    Missing,
    #[error("task execution deadline expired during {phase} (effective budget {budget_ms} ms)")]
    Expired { phase: &'static str, budget_ms: u64 },
}

#[derive(Clone, Default)]
struct Cancellation {
    cancelled: Arc<AtomicBool>,
}

impl Cancellation {
    fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
    }

    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire)
    }
}

struct CancelOnDrop {
    cancellation: Cancellation,
    armed: bool,
}

impl CancelOnDrop {
    fn new(cancellation: Cancellation) -> Self {
        Self {
            cancellation,
            armed: true,
        }
    }

    fn disarm(&mut self) {
        self.armed = false;
    }
}

impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        if self.armed {
            self.cancellation.cancel();
        }
    }
}

#[derive(Clone)]
pub struct TaskDeadline {
    at: tokio::time::Instant,
    budget: Duration,
    cancellation: Cancellation,
}

impl TaskDeadline {
    pub fn from_dispatch(
        absolute_unix_secs: u64,
        max_duration_secs: u32,
    ) -> Result<Self, DeadlineError> {
        Self::from_limits_at(
            SystemTime::now(),
            tokio::time::Instant::now(),
            absolute_unix_secs,
            max_duration_secs,
        )
    }

    fn from_limits_at(
        wall_now: SystemTime,
        monotonic_now: tokio::time::Instant,
        absolute_unix_secs: u64,
        max_duration_secs: u32,
    ) -> Result<Self, DeadlineError> {
        let absolute_remaining = if absolute_unix_secs == 0 {
            None
        } else {
            let absolute = UNIX_EPOCH
                .checked_add(Duration::from_secs(absolute_unix_secs))
                .ok_or_else(|| Self::expired("dispatch receipt", Duration::ZERO))?;
            let remaining = absolute
                .duration_since(wall_now)
                .map_err(|_| Self::expired("dispatch receipt", Duration::ZERO))?;
            if remaining.is_zero() {
                return Err(Self::expired("dispatch receipt", Duration::ZERO));
            }
            Some(remaining)
        };
        let relative =
            (max_duration_secs > 0).then(|| Duration::from_secs(u64::from(max_duration_secs)));
        let budget = match (absolute_remaining, relative) {
            (Some(absolute), Some(relative)) => absolute.min(relative),
            (Some(absolute), None) => absolute,
            (None, Some(relative)) => relative,
            (None, None) => return Err(DeadlineError::Missing),
        };
        if budget.is_zero() {
            return Err(Self::expired("dispatch receipt", budget));
        }
        let at = monotonic_now
            .checked_add(budget)
            .ok_or_else(|| Self::expired("dispatch receipt", budget))?;
        Ok(Self {
            at,
            budget,
            cancellation: Cancellation::default(),
        })
    }

    fn expired(phase: &'static str, budget: Duration) -> DeadlineError {
        DeadlineError::Expired {
            phase,
            budget_ms: u64::try_from(budget.as_millis()).unwrap_or(u64::MAX),
        }
    }

    pub fn check(&self, phase: &'static str) -> Result<(), DeadlineError> {
        if self.cancellation.is_cancelled() || tokio::time::Instant::now() >= self.at {
            self.cancellation.cancel();
            return Err(Self::expired(phase, self.budget));
        }
        Ok(())
    }

    pub async fn run<F, T>(&self, phase: &'static str, future: F) -> Result<T, DeadlineError>
    where
        F: Future<Output = T>,
    {
        self.check(phase)?;
        let mut cancel_on_drop = CancelOnDrop::new(self.cancellation.clone());
        let result = tokio::time::timeout_at(self.at, future).await;
        cancel_on_drop.disarm();
        match result {
            Ok(value) => Ok(value),
            Err(_) => {
                self.cancellation.cancel();
                Err(Self::expired(phase, self.budget))
            }
        }
    }

    #[cfg(test)]
    fn for_test(budget: Duration) -> Self {
        Self {
            at: tokio::time::Instant::now() + budget,
            budget,
            cancellation: Cancellation::default(),
        }
    }

    #[cfg(test)]
    fn is_cancelled(&self) -> bool {
        self.cancellation.is_cancelled()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};

    struct DropSignal(Arc<AtomicBool>);

    impl Drop for DropSignal {
        fn drop(&mut self) {
            self.0.store(true, Ordering::Release);
        }
    }

    #[test]
    fn earliest_server_limit_wins_and_expired_absolute_limit_fails_closed() {
        let wall = UNIX_EPOCH + Duration::from_secs(1_000);
        let mono = tokio::time::Instant::now();
        let relative = TaskDeadline::from_limits_at(wall, mono, 1_030, 5).unwrap();
        assert_eq!(relative.budget, Duration::from_secs(5));

        let absolute = TaskDeadline::from_limits_at(wall, mono, 1_003, 20).unwrap();
        assert_eq!(absolute.budget, Duration::from_secs(3));

        assert!(matches!(
            TaskDeadline::from_limits_at(wall, mono, 999, 20),
            Err(DeadlineError::Expired { .. })
        ));
        assert!(matches!(
            TaskDeadline::from_limits_at(wall, mono, 0, 0),
            Err(DeadlineError::Missing)
        ));
    }

    #[tokio::test]
    async fn timeout_drops_the_inflight_phase_and_cancels_the_deadline() {
        let deadline = TaskDeadline::for_test(Duration::from_millis(20));
        let dropped = Arc::new(AtomicBool::new(false));
        let signal = dropped.clone();
        let result = deadline
            .run("test phase", async move {
                let _drop_signal = DropSignal(signal);
                std::future::pending::<()>().await;
            })
            .await;
        assert!(matches!(result, Err(DeadlineError::Expired { .. })));
        assert!(dropped.load(Ordering::Acquire));
        assert!(deadline.is_cancelled());
    }

    #[tokio::test]
    async fn aborting_the_deadline_future_cancels_and_drops_the_inner_phase() {
        let deadline = TaskDeadline::for_test(Duration::from_secs(30));
        let task_deadline = deadline.clone();
        let dropped = Arc::new(AtomicBool::new(false));
        let signal = dropped.clone();
        let (started_tx, started_rx) = tokio::sync::oneshot::channel();
        let task = tokio::spawn(async move {
            let _ = task_deadline
                .run("cancelled phase", async move {
                    let _drop_signal = DropSignal(signal);
                    let _ = started_tx.send(());
                    std::future::pending::<()>().await;
                })
                .await;
        });
        started_rx.await.unwrap();
        task.abort();
        let _ = task.await;
        tokio::task::yield_now().await;
        assert!(dropped.load(Ordering::Acquire));
        assert!(deadline.is_cancelled());
    }
}
