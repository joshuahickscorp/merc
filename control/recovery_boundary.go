package main

import "context"

type RecoveryBoundary string

const (
	BoundaryCommitAfterTaskProjection       RecoveryBoundary = "commit.after_task_projection"
	BoundaryCommitAfterParentFence          RecoveryBoundary = "commit.after_parent_fence"
	BoundaryCommitAfterJobProjection        RecoveryBoundary = "commit.after_job_projection"
	BoundaryCommitAfterWorkInsert           RecoveryBoundary = "commit.after_work_insert"
	BoundaryCommitBeforeDBCommit            RecoveryBoundary = "commit.before_db_commit"
	BoundaryCommitAfterDBCommit             RecoveryBoundary = "commit.after_db_commit"
	BoundaryVerifyWorkClaimed               RecoveryBoundary = "verify.work_claimed"
	BoundaryVerifyAfterSamplingPin          RecoveryBoundary = "verify.after_sampling_pin"
	BoundaryVerifyAfterStagingRead          RecoveryBoundary = "verify.after_staging_read"
	BoundaryVerifyAfterSealedPut            RecoveryBoundary = "verify.after_sealed_put"
	BoundaryVerifyAfterSealedReadback       RecoveryBoundary = "verify.after_sealed_readback"
	BoundaryVerifyAfterArtifactPin          RecoveryBoundary = "verify.after_artifact_pin"
	BoundaryVerifyAfterDecision             RecoveryBoundary = "verify.after_decision"
	BoundaryApplyAfterEffect                RecoveryBoundary = "verify.apply.after_effect"
	BoundaryAcceptedAfterTask               RecoveryBoundary = "verify.accepted.after_task"
	BoundaryAcceptedAfterVerdict            RecoveryBoundary = "verify.accepted.after_verdict"
	BoundaryAcceptedAfterJobCounter         RecoveryBoundary = "verify.accepted.after_job_counter"
	BoundaryAcceptedAfterSupplierCounter    RecoveryBoundary = "verify.accepted.after_supplier_counter"
	BoundaryAcceptedAfterCounters           RecoveryBoundary = "verify.accepted.after_counters"
	BoundaryAcceptedAfterDuration           RecoveryBoundary = "verify.accepted.after_duration"
	BoundaryAcceptedAfterWorkTerminal       RecoveryBoundary = "verify.accepted.after_work_terminal"
	BoundaryAcceptedAfterLedger             RecoveryBoundary = "verify.accepted.after_ledger"
	BoundaryAcceptedAfterArtifactResolution RecoveryBoundary = "verify.accepted.after_artifact_resolution"
	BoundaryAcceptedAfterSiblingCancel      RecoveryBoundary = "verify.accepted.after_sibling_cancel"
	BoundaryAcceptedBeforeDBCommit          RecoveryBoundary = "verify.accepted.before_db_commit"
	BoundaryAcceptedAfterDBCommit           RecoveryBoundary = "verify.accepted.after_db_commit"
	BoundaryRejectedAfterVerdict            RecoveryBoundary = "verify.rejected.after_verdict"
	BoundaryRejectedAfterRequeue            RecoveryBoundary = "verify.rejected.after_requeue"
	BoundaryRejectedAfterParentRunning      RecoveryBoundary = "verify.rejected.after_parent_running"
	BoundaryRejectedAfterWorkTerminal       RecoveryBoundary = "verify.rejected.after_work_terminal"
	BoundaryRejectedBeforeDBCommit          RecoveryBoundary = "verify.rejected.before_db_commit"
	BoundaryRejectedAfterDBCommit           RecoveryBoundary = "verify.rejected.after_db_commit"
	BoundaryMergeBeforePut                  RecoveryBoundary = "merge.before_put"
	BoundaryMergeAfterPut                   RecoveryBoundary = "merge.after_put"
	BoundaryMergeAfterVerify                RecoveryBoundary = "merge.after_verify"
	BoundaryMergeBeforePublish              RecoveryBoundary = "merge.before_publish"
	BoundaryMergeAfterPublish               RecoveryBoundary = "merge.after_publish"
	BoundaryCompleteAfterJobProjection      RecoveryBoundary = "complete.after_job_projection"
	BoundaryCompleteAfterSLAPremium         RecoveryBoundary = "complete.after_sla_premium"
	BoundaryCompleteAfterActualUSD          RecoveryBoundary = "complete.after_actual_usd"
	BoundaryCompleteBeforeDBCommit          RecoveryBoundary = "complete.before_db_commit"
	BoundaryCompleteAfterDBCommit           RecoveryBoundary = "complete.after_db_commit"
)

type recoveryBoundaryProbe interface {
	Reach(context.Context, RecoveryBoundary)
}

func reachRecoveryBoundary(ctx context.Context, probe recoveryBoundaryProbe, boundary RecoveryBoundary) {
	if probe != nil {
		probe.Reach(ctx, boundary)
	}
}
