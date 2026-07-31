package main

import "fmt"

// Governed verification classes.
//
// Ordinary production sampling is unchanged: a primary task is checked with a
// reputation-derived probability, HMAC-selected per task id, and that is the
// right economics for buyer traffic. What was missing is a way to say "this
// particular execution WILL be verified" that does not involve changing the
// sampling secret.
//
// Two things went wrong without it. Proof and canary runs were graded by a coin
// flip, so a receipt could honestly record "no verification outcome" for a task
// whose entire purpose was to produce one. And a test that required a terminal
// outcome passed or failed on that same coin flip: it asserted nothing about the
// code either way, and the fix in the other direction — accepting "no outcome" —
// asserted nothing either.
//
// Changing MERC_VERIFICATION_SAMPLE_SECRET to force selection is not a fix. It
// makes one binary's sampling deterministic while pretending the class of work
// did not change, and it moves the secret that decides every OTHER task's fate.
const (
	// VerificationClassSampled is ordinary buyer traffic. Selection is
	// probabilistic and reputation-weighted; this is the only class a buyer may
	// ask for, and the only one that may be selected by default.
	VerificationClassSampled = "SAMPLED"
	// VerificationClassRequired is always checked. Reserved for system and
	// operator work: proof runs, canary cohorts, directed experiments.
	VerificationClassRequired = "REQUIRED"
	// VerificationClassHoneypot is a known-answer probe. Always checked — the
	// answer is already stored and the comparison is a byte compare, so sampling
	// it would save nothing and would disarm the probe at random.
	VerificationClassHoneypot = "HONEYPOT"
	// VerificationClassRedundant is a second execution of a primary chunk,
	// compared against its peer. Always checked: the second execution was
	// purchased in order to be compared.
	VerificationClassRedundant = "REDUNDANT"
	// VerificationClassReplay re-executes a recorded input to compare against a
	// recorded output. Always checked, for the same reason.
	VerificationClassReplay = "REPLAY"
)

// verificationClassPolicyRevision identifies the RULE SET that assigned classes,
// separately from the sampling secret that resolves SAMPLED into a decision.
//
// Bound into the task, the compute plan, the verification work row and the
// receipt, so a stored decision can be re-read against the rules that produced it
// rather than against whatever the rules say today.
const verificationClassPolicyRevision = "verify-class-v1"

func knownVerificationClass(class string) bool {
	switch class {
	case VerificationClassSampled, VerificationClassRequired,
		VerificationClassHoneypot, VerificationClassRedundant, VerificationClassReplay:
		return true
	}
	return false
}

// verificationClassAlwaysChecked reports whether this class removes the coin
// flip. Everything except ordinary sampling does.
func verificationClassAlwaysChecked(class string) bool {
	return knownVerificationClass(class) && class != VerificationClassSampled
}

// deriveTaskVerificationClass assigns the class a task actually carries.
//
// The probe and redundancy flags decide it outright: those tasks exist in order
// to be compared, and a HONEYPOT that sampling declined to check is a probe that
// was never armed. Everything else takes the job's governed class, which is
// SAMPLED unless system or operator authority set otherwise.
func deriveTaskVerificationClass(jobClass string, isHoneypot, isRedundancy bool) string {
	switch {
	case isHoneypot:
		return VerificationClassHoneypot
	case isRedundancy:
		return VerificationClassRedundant
	case jobSelectableVerificationClass(jobClass):
		return jobClass
	default:
		// Includes HONEYPOT and REDUNDANT asked for job-wide, which is not a
		// misconfiguration to be honoured: a primary task labelled HONEYPOT would
		// be graded against an answer that does not exist for it. Falls back to
		// ordinary sampling rather than to certainty.
		return VerificationClassSampled
	}
}

// jobSelectableVerificationClass is the closed set that can be a JOB's class.
// HONEYPOT and REDUNDANT are properties of individual tasks — the probe and
// redundancy flags assign them — and are excluded here on purpose.
func jobSelectableVerificationClass(class string) bool {
	switch class {
	case VerificationClassSampled, VerificationClassRequired, VerificationClassReplay:
		return true
	}
	return false
}

// validateTaskVerificationClass refuses a class that contradicts the task it is
// attached to. A primary task labelled HONEYPOT would be checked against an
// answer that does not exist for it.
func validateTaskVerificationClass(class string, isHoneypot, isRedundancy bool) error {
	if !knownVerificationClass(class) {
		return fmt.Errorf("unknown verification class %q", class)
	}
	if want := deriveTaskVerificationClass(class, isHoneypot, isRedundancy); want != class {
		return fmt.Errorf(
			"task is labelled verification class %q but its honeypot=%v redundancy=%v flags make it %q",
			class, isHoneypot, isRedundancy, want)
	}
	return nil
}

// governComputePlanVerificationClass stamps a governed class onto a frozen plan.
//
// Separate from plan construction because the class is not part of the priced
// geometry: REQUIRED verifies the same executions rather than buying more of
// them, so it changes no task count, no memory floor and no ETA. Threading it
// through a fourteen-argument constructor would suggest otherwise.
func governComputePlanVerificationClass(plan ComputePlan, class string) (ComputePlan, error) {
	if !knownVerificationClass(class) {
		return ComputePlan{}, fmt.Errorf("unknown verification class %q", class)
	}
	// HONEYPOT and REDUNDANT describe individual tasks, not a whole job: a job
	// whose every primary task were a honeypot would have no buyer work in it.
	if class == VerificationClassHoneypot || class == VerificationClassRedundant {
		return ComputePlan{}, fmt.Errorf(
			"verification class %q is assigned per task by the probe and redundancy "+
				"flags, not per job", class)
	}
	plan.VerificationClass = class
	plan.VerificationClassPolicy = verificationClassPolicyRevision
	return plan, nil
}
