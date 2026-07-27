package main

import (
	"math"
	"net/http"
	"strings"
)

// normalizeAndValidateJobSubmit applies every check on a submission that depends
// only on the request itself, returning the normalized submission or the error
// the handler should send.
//
// These branches used to live inline in createJob, which is ~500 lines and needs
// a live Postgres and MinIO to construct. That meant the validation matrix --
// the part most likely to be edited and most likely to be wrong -- had no test
// coverage beyond the first dozen lines. Nothing here touches the database, the
// object store, or the clock, so it is testable directly.
//
// Checks that need the store (intake pause, canary admission counters, billing)
// deliberately stay in createJob: they are not pure and pretending otherwise
// would just move the untestable part somewhere less obvious.
func normalizeAndValidateJobSubmit(sub jobSubmit) (jobSubmit, *httpError) {
	if math.IsNaN(sub.MaxUSD) || math.IsInf(sub.MaxUSD, 0) || sub.MaxUSD < 0 {
		return sub, &httpError{http.StatusBadRequest, "max_usd must be a finite non-negative number"}
	}
	if sub.JobType.Type == "" {
		return sub, &httpError{http.StatusBadRequest, "job_type.type is required"}
	}
	if !validJobTypes[sub.JobType.Type] {
		return sub, &httpError{http.StatusBadRequest, "invalid job_type.type: " + sub.JobType.Type}
	}
	if sub.Tier == "" {
		sub.Tier = "batch"
	}
	if !validTiers[sub.Tier] {
		return sub, &httpError{http.StatusBadRequest, "invalid tier: " + sub.Tier}
	}
	canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
	if err != nil {
		return sub, &httpError{http.StatusBadRequest, err.Error()}
	}
	sub.Model = canonicalModel

	// Reject a temperature the engine will not honour, everywhere -- not only
	// under canary, which already refuses it.
	//
	// The executor decodes with argmax in both the single and batched paths
	// (agent/src/executor.rs), so a non-zero temperature was accepted by the API,
	// carried through the manifest, and silently discarded. Accepting a parameter
	// and ignoring it is the worst of the three options: the buyer believes they
	// asked for sampling and is billed for greedy output.
	//
	// Implementing sampling instead would be actively wrong here: redundancy
	// verification compares outputs across workers, so non-deterministic decoding
	// would make honest workers disagree. Deterministic decoding is a property of
	// the product, not a limitation to paper over.
	if sub.JobType.Temperature != 0 {
		return sub, &httpError{http.StatusBadRequest,
			"temperature must be 0: verified execution decodes deterministically so " +
				"redundant workers can be compared, and sampling would make honest " +
				"workers disagree"}
	}
	for _, c := range sub.Constraints.HWClasses {
		if !validHWClasses[c] {
			return sub, &httpError{http.StatusBadRequest, "invalid hw_class: " + c}
		}
	}
	if sub.WebhookURL != "" {
		sub.WebhookURL = strings.TrimSpace(sub.WebhookURL)
		if _, err := validateWebhookURLSyntax(sub.WebhookURL, false); err != nil {
			return sub, &httpError{http.StatusBadRequest, err.Error()}
		}
		if err := requireWebhookSigningKey(); err != nil {
			return sub, &httpError{http.StatusServiceUnavailable,
				"webhook registration unavailable: encrypted signing-secret storage is not configured"}
		}
	}
	if sub.DeadlineSecs != 0 && sub.DeadlineSecs != -1 &&
		(sub.DeadlineSecs < 60 || sub.DeadlineSecs > 604800) {
		return sub, &httpError{http.StatusBadRequest,
			"deadline_secs must be -1 (run to completion), 0 (default watchdog), or 60..604800 seconds"}
	}
	// Unconditional supplier-safety ceiling; canary may already be stricter.
	sub.Constraints.MaxDurationSecs = clampMaxDurationSecs(sub.Constraints.MaxDurationSecs)
	return sub, nil
}
