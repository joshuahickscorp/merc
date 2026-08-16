package main

import (
	"math"
	"net/http"
	"strings"
)

// normalizeWorkloadRequest is the single request-shape gate used by both quote
// and submit. Canary verification floors are part of the normalized shape:
// otherwise a quote would commit to fewer tasks than submit is allowed to run.
//
// Callers may pass false for the pre-database structural pass. That pass runs
// every request-only check but deliberately does not consult the process's
// advertised runtime cache. The default/full pass must run only after
// activationForNewAdmission has refreshed the cache from PostgreSQL.
func (s *Server) normalizeWorkloadRequest(
	sub jobSubmit, validateRuntimeAuthority ...bool,
) (jobSubmit, *httpError) {
	if err := s.canary.validateJobShape(sub); err != nil {
		return sub, &httpError{http.StatusForbidden, "outside private-canary limits: " + err.Error()}
	}
	if s.canary.Enabled {
		if sub.Verification.RedundancyFrac < 1 {
			sub.Verification.RedundancyFrac = 1
		}
		// Binary media has no JSONL known-answer corpus. The media runner's
		// independent byte-exact execution is the canary verifier; injecting a
		// text honeypot here would make a valid media request unquotable.
		if !isBinaryMediaJob(sub) && sub.Verification.HoneypotFrac <= 0 {
			sub.Verification.HoneypotFrac = 0.1
		} else if isBinaryMediaJob(sub) {
			sub.Verification.HoneypotFrac = 0
		}
		if sub.Verification.PayoutHoldSecs < 7*24*60*60 {
			sub.Verification.PayoutHoldSecs = 7 * 24 * 60 * 60
		}
	}
	return normalizeAndValidateJobSubmit(sub, validateRuntimeAuthority...)
}

// normalizeAndValidateJobSubmit applies every request-only check and, by
// default, binds the model to the currently installed advertised runtime
// authority. Passing false skips only that authority lookup for the structural
// preflight described above.
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
func normalizeAndValidateJobSubmit(
	sub jobSubmit, validateRuntimeAuthority ...bool,
) (jobSubmit, *httpError) {
	checkRuntimeAuthority := len(validateRuntimeAuthority) == 0 ||
		validateRuntimeAuthority[0]
	if math.IsNaN(sub.MaxUSD) || math.IsInf(sub.MaxUSD, 0) || sub.MaxUSD < 0 {
		return sub, &httpError{http.StatusBadRequest, "max_usd must be a finite non-negative number"}
	}
	if !validFiniteUnitFloat(sub.MinReputation) {
		return sub, &httpError{http.StatusBadRequest, "min_reputation must be a finite number in [0,1]"}
	}
	minMemory := float64(sub.Constraints.MinMemoryGB)
	if math.IsNaN(minMemory) || math.IsInf(minMemory, 0) || minMemory < 0 {
		return sub, &httpError{http.StatusBadRequest, "constraints.min_memory_gb must be a finite non-negative number"}
	}
	if !validFiniteUnitFloat(sub.Verification.RedundancyFrac) {
		return sub, &httpError{http.StatusBadRequest, "verification.redundancy_frac must be a finite number in [0,1]"}
	}
	if !validFiniteUnitFloat(sub.Verification.HoneypotFrac) {
		return sub, &httpError{http.StatusBadRequest, "verification.honeypot_frac must be a finite number in [0,1]"}
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
	objective, err := canonicalWorkloadObjective(sub.Objective)
	if err != nil {
		return sub, &httpError{http.StatusBadRequest, err.Error()}
	}
	sub.Objective = objective
	if checkRuntimeAuthority {
		canonicalModel, err := normalizeAdvertisedRuntimeModelRef(sub.JobType.Type, sub.Model)
		if err != nil {
			return sub, &httpError{http.StatusBadRequest, err.Error()}
		}
		sub.Model = canonicalModel
	}

	switch sub.JobType.Type {
	case "embed":
		if sub.JobType.MaxTokens != 0 || sub.JobType.Temperature != 0 {
			return sub, &httpError{http.StatusBadRequest,
				"embed workload cannot carry generation-only max_tokens or temperature fields"}
		}
		if sub.JobType.BatchSize < 0 || sub.JobType.BatchSize > maxEmbedBatchSize {
			return sub, &httpError{http.StatusBadRequest,
				"embed batch_size must be in [0,4096]"}
		}
	case "batch_infer":
		if sub.JobType.BatchSize != 0 || sub.JobType.EmbedBinary {
			return sub, &httpError{http.StatusBadRequest,
				"batch_infer workload cannot carry embedding-only batch_size or binary fields"}
		}
	case "media_transcode":
		if err := normalizeMediaJobType(&sub.JobType); err != nil {
			return sub, &httpError{http.StatusBadRequest, err.Error()}
		}
		// Media has no known-answer corpus. Its bounded runtime verifier is the
		// agent's ffprobe contract plus byte-exact independent redundancy, so a
		// media request must carry that redundancy and may not silently fall back
		// to a JSONL honeypot.
		if sub.Verification.HoneypotFrac > 0 {
			return sub, &httpError{http.StatusBadRequest,
				"media_transcode does not support honeypot verification; use independent redundancy"}
		}
		if sub.Verification.RedundancyFrac <= 0 {
			sub.Verification.RedundancyFrac = 1
		}
	case "media_rendering":
		if err := normalizeRenderingJobType(&sub.JobType); err != nil {
			return sub, &httpError{http.StatusBadRequest, err.Error()}
		}
		if sub.Verification.HoneypotFrac > 0 {
			return sub, &httpError{http.StatusBadRequest, "media_rendering does not support honeypot verification; use independent redundancy"}
		}
		if sub.Verification.RedundancyFrac <= 0 {
			sub.Verification.RedundancyFrac = 1
		}
	}

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
	params, err := canonicalizeJobParams(sub.Params)
	if err != nil {
		return sub, &httpError{http.StatusBadRequest, err.Error()}
	}
	sub.Params = params

	sub.Constraints.HWClasses = normalizeUniqueStrings(
		sub.Constraints.HWClasses, strings.TrimSpace,
	)
	for _, c := range sub.Constraints.HWClasses {
		if !validHWClasses[c] {
			return sub, &httpError{http.StatusBadRequest, "invalid hw_class: " + c}
		}
	}
	sub.Constraints.DataResidency = normalizeUniqueStrings(
		sub.Constraints.DataResidency,
		func(value string) string { return strings.ToUpper(strings.TrimSpace(value)) },
	)
	for _, country := range sub.Constraints.DataResidency {
		if !validDataCountry(country) {
			return sub, &httpError{http.StatusBadRequest,
				"data_residency entries must be two-letter ISO country codes"}
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
