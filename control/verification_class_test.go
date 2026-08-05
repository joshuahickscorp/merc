package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A store that reports one reputation, so the sampling arithmetic is the only
// variable. Everything else refuses to be called: a class test that silently
// reached a honeypot lookup or a redundancy peer selection would be testing
// something else.
type reputationOnlyVerificationStore struct {
	verificationStore
	worker     uuid.UUID
	reputation float32
}

func (r reputationOnlyVerificationStore) CandidateWorkers(
	context.Context, string, string, float32,
) ([]MatchWorker, error) {
	return []MatchWorker{{ID: r.worker, Reputation: r.reputation}}, nil
}

// The defect this exists for: proof and canary work was graded by a coin flip.
//
// A trusted supplier's ordinary task is checked with probability 0.25, which is
// correct economics for buyer traffic and useless for a run whose entire purpose
// is to produce a verification outcome. Every governed class returns certainty,
// and it does so as a probability of 1.0 rather than as a branch around the
// sampling machinery — so the pinned sampling row still records what actually
// decided the task instead of a reputation number that had no bearing on it.
func TestGovernedVerificationClassesRemoveTheCoinFlip(t *testing.T) {
	worker := uuid.New()
	verifier := &Verifier{
		store:        reputationOnlyVerificationStore{worker: worker, reputation: 1.0},
		sampleSecret: []byte("a-strong-verification-sample-secret-32+"),
	}
	info := &CommitTaskInfo{WorkerID: worker, jobType: "embed", ModelRef: "all-minilm-l6-v2"}

	info.VerificationClass = VerificationClassSampled
	sampled := verifier.effectiveCheckProb(context.Background(), info)
	if sampled >= 1.0 {
		t.Fatalf("ordinary sampling of a fully trusted supplier returned probability %v; "+
			"the governed classes would then be indistinguishable from it", sampled)
	}
	if sampled != verifyCheckProbFloor {
		t.Errorf("ordinary sampling probability %v, want the trust floor %v",
			sampled, verifyCheckProbFloor)
	}

	for _, class := range []string{
		VerificationClassRequired, VerificationClassHoneypot,
		VerificationClassRedundant, VerificationClassReplay,
	} {
		info.VerificationClass = class
		if got := verifier.effectiveCheckProb(context.Background(), info); got != 1.0 {
			t.Errorf("class %s returned probability %v, want 1.0", class, got)
		}
		if !verifier.checkSampled(info.TaskID, 1.0) {
			t.Errorf("class %s was not selected at probability 1.0", class)
		}
	}

	// An unknown class must NOT be treated as governed. Fail closed toward
	// ordinary sampling rather than toward free certainty.
	info.VerificationClass = "DEFINITELY_NOT_A_CLASS"
	if got := verifier.effectiveCheckProb(context.Background(), info); got == 1.0 {
		t.Error("an unrecognised class bought guaranteed verification")
	}
}

// The class is a system decision, so it must not be reachable from a buyer
// request at all. Submit decodes with DisallowUnknownFields, so the field simply
// does not exist on the wire — which is a stronger guarantee than a validator
// that has to remember to refuse it.
func TestBuyerCannotSelectAVerificationClassOnTheWire(t *testing.T) {
	for name, body := range map[string]string{
		"inside the verification policy": `{"verification":{"class":"REQUIRED"}}`,
		"at the top level":               `{"verification_class":"REQUIRED"}`,
		"as a governed field":            `{"governed_verification_class":"REQUIRED"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var sub jobSubmit
			err := decodeStrictJSONObject([]byte(body), &sub)
			if err == nil {
				t.Fatal("a buyer request selected a verification class")
			}
			if sub.governedVerificationClass != "" {
				t.Fatalf("the wire set the governed class to %q", sub.governedVerificationClass)
			}
		})
	}
}

// A class assigned per task may not be claimed job-wide. A job whose every
// primary task were a honeypot would contain no buyer work at all.
func TestPerTaskClassesCannotBeClaimedJobWide(t *testing.T) {
	for _, class := range []string{VerificationClassHoneypot, VerificationClassRedundant} {
		if _, err := governComputePlanVerificationClass(ComputePlan{}, class); err == nil {
			t.Errorf("a compute plan claimed job-wide class %s", class)
		}
	}
	if _, err := governComputePlanVerificationClass(ComputePlan{}, "NONSENSE"); err == nil {
		t.Error("a compute plan claimed an unknown class")
	}
	plan, err := governComputePlanVerificationClass(ComputePlan{}, VerificationClassRequired)
	must(t, err)
	if plan.VerificationClass != VerificationClassRequired ||
		plan.VerificationClassPolicy != verificationClassPolicyRevision {
		t.Fatalf("stamped plan = %+v", plan)
	}
}

// A class must not contradict the flags on the task it labels, and the rule lives
// in the database as well as in Go — a caller that bypasses the Go path cannot
// make a primary task claim to be a probe.
func TestTaskVerificationClassCannotContradictItsFlags(t *testing.T) {
	if err := validateTaskVerificationClass(VerificationClassHoneypot, false, false); err == nil {
		t.Error("a primary task was labelled HONEYPOT")
	}
	if err := validateTaskVerificationClass(VerificationClassSampled, true, false); err == nil {
		t.Error("a probe was labelled SAMPLED and could be disarmed by sampling")
	}
	if err := validateTaskVerificationClass(VerificationClassRequired, false, false); err != nil {
		t.Errorf("a governed primary task was refused: %v", err)
	}

	// The database DERIVES the class rather than refusing a caller who did not
	// set it, because production paths mark a task a probe after the row exists.
	// A trigger normalises; the CHECK then asserts what the trigger guarantees.
	ctx, store, pool := openIsolatedTestStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]

	for _, tc := range []struct {
		name, statement, want string
	}{
		{"marking a task a probe makes it one",
			`UPDATE tasks SET is_honeypot=true WHERE id=$1`, VerificationClassHoneypot},
		{"unmarking it returns it to ordinary sampling",
			`UPDATE tasks SET is_honeypot=false WHERE id=$1`, VerificationClassSampled},
		{"a governed class on a primary task is kept",
			`UPDATE tasks SET verification_class='REQUIRED' WHERE id=$1`, VerificationClassRequired},
		{"a primary task cannot claim to be a probe",
			`UPDATE tasks SET verification_class='HONEYPOT' WHERE id=$1`, VerificationClassSampled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tc.statement, taskID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var got string
			var honeypot bool
			if err := pool.QueryRow(ctx,
				`SELECT verification_class, is_honeypot FROM tasks WHERE id=$1`, taskID).
				Scan(&got, &honeypot); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("stored class %q, want %q", got, tc.want)
			}
			mustf(t, validateTaskVerificationClass(got, honeypot, false), "the database stored a class its own validator refuses: %v")
		})
	}
}

// Derivation is what every task insert goes through, so the mapping from flags to
// class has to be exactly the mapping the database CHECK enforces.
func TestTaskVerificationClassDerivation(t *testing.T) {
	for _, tc := range []struct {
		jobClass             string
		honeypot, redundancy bool
		want                 string
	}{
		{"", false, false, VerificationClassSampled},
		{VerificationClassSampled, false, false, VerificationClassSampled},
		{VerificationClassRequired, false, false, VerificationClassRequired},
		{VerificationClassReplay, false, false, VerificationClassReplay},
		// The flags win. A probe on a REQUIRED job is still a probe.
		{VerificationClassRequired, true, false, VerificationClassHoneypot},
		{VerificationClassRequired, false, true, VerificationClassRedundant},
		{"", true, false, VerificationClassHoneypot},
		{"", false, true, VerificationClassRedundant},
		// An unrecognised job class falls back to ordinary sampling rather than
		// to certainty.
		{"NONSENSE", false, false, VerificationClassSampled},
	} {
		got := deriveTaskVerificationClass(tc.jobClass, tc.honeypot, tc.redundancy)
		if got != tc.want {
			t.Errorf("job=%q honeypot=%v redundancy=%v -> %q, want %q",
				tc.jobClass, tc.honeypot, tc.redundancy, got, tc.want)
		}
		if err := validateTaskVerificationClass(got, tc.honeypot, tc.redundancy); err != nil {
			t.Errorf("derivation produced a class its own validator refuses: %v", err)
		}
	}
}

// The class is governed authority, so a plan carrying one must say under which
// rules it was assigned. ValidateFrozenComputePlanSnapshot calls this on every
// frozen plan.
func TestComputePlanVerificationClassRules(t *testing.T) {
	for name, tc := range map[string]struct {
		plan    ComputePlan
		refused string
	}{
		"no class at all": {ComputePlan{}, ""},
		"ordinary class with its policy": {ComputePlan{
			VerificationClass:       VerificationClassSampled,
			VerificationClassPolicy: verificationClassPolicyRevision,
		}, ""},
		"governed class with its policy": {ComputePlan{
			VerificationClass:       VerificationClassRequired,
			VerificationClassPolicy: verificationClassPolicyRevision,
		}, ""},
		"class with no policy": {
			ComputePlan{VerificationClass: VerificationClassRequired}, "policy revision"},
		"policy with no class": {
			ComputePlan{VerificationClassPolicy: verificationClassPolicyRevision}, "with no class"},
		"unknown class": {ComputePlan{
			VerificationClass:       "NONSENSE",
			VerificationClassPolicy: verificationClassPolicyRevision,
		}, "unknown verification class"},
		"per-task class claimed job-wide": {ComputePlan{
			VerificationClass:       VerificationClassHoneypot,
			VerificationClassPolicy: verificationClassPolicyRevision,
		}, "per-task verification class"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateComputePlanVerificationClass(tc.plan)
			switch {
			case tc.refused == "" && err != nil:
				t.Fatalf("refused a valid plan: %v", err)
			case tc.refused != "" && err == nil:
				t.Fatalf("accepted a plan that should be refused")
			case tc.refused != "" && !strings.Contains(err.Error(), tc.refused):
				t.Fatalf("refused with %q, want a message containing %q", err, tc.refused)
			}
		})
	}
}

// The class through the production commit and verification path, not through a
// unit-level probability.
//
// What must be visible afterwards is not only that the check ran, but that the
// stored record says WHY it ran: probability 1 and selected true, with the class
// beside them, so a reader can tell "certain because governed" from "certain
// because the supplier is new".
func TestRequiredClassIsPinnedSelectedThroughTheRealProcessor(t *testing.T) {
	c := newFailureCaseWithClass(t, VerificationClassRequired)
	c.claim(t)
	info := c.h.commitThroughStorage(c.ctx, c.f, c.task, c.body)
	if info.VerificationClass != VerificationClassRequired {
		t.Fatalf("commit read the task's class as %q", info.VerificationClass)
	}

	if _, err := c.h.processor.ProcessAttempt(c.ctx, c.task, 0); err != nil {
		t.Fatalf("verification: %v", err)
	}

	var class string
	var probability *string
	var selected *bool
	var outcome *string
	if err := c.h.pool().QueryRow(c.ctx, `
		SELECT verification_class, sampling_probability, sampling_selected, terminal_outcome
		  FROM verification_work WHERE task_id=$1 AND attempt=0`, c.task).
		Scan(&class, &probability, &selected, &outcome); err != nil {
		t.Fatalf("read verification work: %v", err)
	}
	t.Logf("class=%s probability=%v selected=%v outcome=%v",
		class, derefOr(probability), selected, derefOr(outcome))

	if class != VerificationClassRequired {
		t.Errorf("verification work recorded class %q", class)
	}
	if selected == nil || !*selected {
		t.Error("a REQUIRED task was not selected for checking")
	}
	if probability == nil || *probability != "1" {
		t.Errorf("sampling probability recorded as %v, want 1", derefOr(probability))
	}
	// The point of the class: a terminal outcome exists, every time, so a test
	// asserting one is not asserting a coin flip.
	if outcome == nil || *outcome == "" {
		t.Fatal("a REQUIRED task reached no terminal verification outcome")
	}

	// And the receipt carries it, so a buyer can tell a task that was not checked
	// from one whose check has not happened yet.
	receipts, err := c.store.JobTaskReceipts(c.ctx, c.f.JobID)
	mustf(t, err, "task receipts: %v")
	if len(receipts) == 0 {
		t.Fatal("no task receipts")
	}
	for _, receipt := range receipts {
		if receipt.VerificationClass != VerificationClassRequired {
			t.Errorf("receipt chunk %d records class %q",
				receipt.ChunkIndex, receipt.VerificationClass)
		}
		if receipt.VerificationSelected == nil || !*receipt.VerificationSelected {
			t.Errorf("receipt chunk %d does not record that the check ran", receipt.ChunkIndex)
		}
	}
}
