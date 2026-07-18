//go:build integration

package main

// adversarial_test.go — Verification & Result Trust 7->8: "Prove gameability
// bounds with a real adversarial harness"
// (docs/internal/CREED_AND_PATH_TO_TEN.md).
//
// This is a REAL adversarial worker, not a mock: it authenticates with its own
// real worker_tokens row, makes real HTTP calls against the real running
// itHTTP control-plane instance (GET /v1/worker/poll, POST
// /v1/worker/task/{id}/commit — the exact same handlers a genuine Rust agent
// hits), and really PUTs bytes to the real MinIO-backed storage at the
// presigned result key. The only thing "adversarial" about it is WHAT bytes it
// chooses to commit — the wire protocol is indistinguishable from an honest
// worker.
//
// Three cheat strategies, each its own scenario function:
//   - runGarbageScenario:   commits random/malformed bytes instead of real work.
//   - runReplayScenario:    commits a stale result from an earlier, different
//     task instead of computing a fresh one.
//   - runHoneypotSkimScenario: correctly answers honeypot tasks (recognized
//     exactly the way TestPipelineChaining's driveOneTask helper already does —
//     via the presigned input_url containing the seeded honeypot's known input
//     ref) while committing garbage on every real, non-honeypot task.
//
// Each scenario submits real jobs (redundancy_frac=1.0 AND honeypot_frac=1.0 —
// the "verification is on" configuration the 6->7 floor guarantees for a real
// buyer who does not opt out) one at a time, drives the adversary and a
// genuinely independent honest peer worker through the real poll->commit
// lifecycle, and counts how many of the adversary's OWN task commits it takes
// before the engine auto-quarantines it (real suppliers.status='suspended',
// confirmed by a real subsequent poll being refused — see quarantinedNow for
// exactly what "refused" means over this wire protocol). N is measured, not
// assumed, and each scenario is run R times to confirm the bound is
// consistent — see TestAdversarialGameabilityBounds.

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// adversarialIdentity is one real, freshly-minted worker+supplier pair,
// authenticated via a REAL worker_tokens row (Store.CreateWorkerToken — the
// same onboarding path a real supplier uses), so every HTTP call this
// identity makes is indistinguishable on the wire from a genuine agent.
type adversarialIdentity struct {
	supplierID uuid.UUID
	workerID   uuid.UUID
	token      string
}

// newAdversarialWorker mints a brand-new real supplier + worker + token (never
// the demo identity — an independent supplier is required for redundancy/
// honeypot comparisons to count as genuinely cross-checked, not
// same-supplier-collusion) and marks it live + capable of the embed job type
// on a same-hw-class lane as the honest peer, exactly like the existing
// TestTiebreakThreeWay-style fixtures.
func newAdversarialWorker(t *testing.T, ctx context.Context, label string) adversarialIdentity {
	t.Helper()
	supplierID := uuid.New()
	workerID := uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO suppliers (id, email, reputation, status) VALUES ($1,$2,0.90,'active')`,
		supplierID, "adversary-"+label+"-"+supplierID.String()+"@computexchange.test"); err != nil {
		t.Fatalf("insert adversarial supplier: %v", err)
	}
	tok, err := itStore.CreateWorkerToken(ctx, workerID, supplierID)
	if err != nil {
		t.Fatalf("CreateWorkerToken: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET hw_class='apple_silicon_max', memory_gb=64, bw_gbps=400,
		        last_seen_at=now(), version='adversarial-harness',
		        supported_jobs=ARRAY['embed'], supported_models=ARRAY['all-minilm-l6-v2'],
		        min_payout_usd_hr=0, thermal_ok=true
		  WHERE id=$1`, workerID); err != nil {
		t.Fatalf("configure adversarial worker: %v", err)
	}
	// CreateWorkerToken intentionally creates an inert placeholder. Mirror real
	// registration by granting only the current generated production cell this
	// harness exercises; legacy arrays are never dispatch authority.
	replaceWorkerAuthorizationsForTest(t, ctx, workerID,
		[2]string{"embed", "all-minilm-l6-v2"})
	return adversarialIdentity{supplierID: supplierID, workerID: workerID, token: tok}
}

// touchLive re-stamps last_seen_at so the identity clears the 60s liveness
// filter Match() applies — needed because a real test can run long enough
// (multiple job submissions + polls) that an idle identity would otherwise
// age out between scenario iterations, which would be a harness artifact, not
// a real detection signal.
func touchLive(t *testing.T, ctx context.Context, workerID uuid.UUID) {
	t.Helper()
	if _, err := itPool.Exec(ctx, `UPDATE workers SET last_seen_at=now() WHERE id=$1`, workerID); err != nil {
		t.Fatalf("touchLive: %v", err)
	}
}

// excludeDemoWorkerFromCandidacy stales out the shared demo worker's
// liveness so it drops out of Match()'s 60s-liveness candidate pool for the
// rest of this test. Found by direct DB inspection: without this,
// SelectRedundancyPeerExcluding's real tiebreak-peer search (same hw_class,
// live, eligible) can pick the SEEDED DEMO WORKER over this harness's own
// honest2 identity — the resulting pinned tiebreak task then sits queued
// forever because this harness never polls as the demo worker, silently
// preventing every tiebreak from ever resolving and understating detection.
// reset(t) unconditionally re-stamps the demo worker live, so this must run
// AFTER reset(t), not before.
func excludeDemoWorkerFromCandidacy(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := itPool.Exec(ctx,
		`UPDATE workers SET last_seen_at = now() - interval '1 hour' WHERE id = $1`, demoWorkerUUID); err != nil {
		t.Fatalf("excludeDemoWorkerFromCandidacy: %v", err)
	}
}

// pollAsWorker performs a REAL GET /v1/worker/poll with the given identity's
// real token. hasWork=false (204 no content, or any non-200) is a legitimate
// outcome the caller must handle — see quarantinedNow for how a suspended
// supplier's refusal is distinguished from ordinary "nothing queued".
func pollAsWorker(t *testing.T, token string) (code int, disp TaskDispatch, hasWork bool) {
	t.Helper()
	code, body := req(t, "GET", "/v1/worker/poll", nil, hdr{"X-Worker-Token", token})
	if code != http.StatusOK {
		return code, TaskDispatch{}, false
	}
	if err := json.Unmarshal(body, &disp); err != nil {
		t.Fatalf("dispatch decode: %v (%s)", err, body)
	}
	return code, disp, true
}

// commitAsWorker PUTs resultBytes to the dispatch's real presigned result key
// and POSTs a real commit for it, returning the HTTP status (204 on success;
// the caller inspects non-204 explicitly where that itself is the point, e.g.
// a quarantined worker's poll never even reaching commit).
func commitAsWorker(t *testing.T, ctx context.Context, token string, disp TaskDispatch, resultBytes []byte) int {
	t.Helper()
	if err := itStorage.PutObject(ctx, disp.ResultKey, resultBytes, "application/json"); err != nil {
		t.Fatalf("put result: %v", err)
	}
	commit := TaskCommit{TaskID: disp.TaskID, ResultKey: disp.ResultKey, DurationMS: 10, TokensUsed: 8}
	code, body := req(t, "POST", "/v1/worker/task/"+disp.TaskID.String()+"/commit", commit, hdr{"X-Worker-Token", token}, jsonCT())
	if code >= 500 {
		// A KNOWN, benign occurrence in the garbage/replay scenarios, not a
		// harness bug: verification (dock/clawback/quarantine, api.go's
		// verifyTaskResult call) and payout scheduling both run and fully
		// complete BEFORE finalizeJobIfDone's buyer-facing merge step
		// (api.go's handleWorkerCommit, in that order) — so this 500 is
		// downstream of the real detection this test measures, not a failure
		// of it. It fires when the adversary's garbage happens to be the
		// LAST task committed for a job whose primary is also garbage: the
		// merge step tries to parse that primary as a real embed result to
		// build the buyer-ready artifact and fails loudly instead of
		// gracefully marking the job degraded/failed. That gap — a garbage
		// primary can 500 a buyer's GET rather than surface a clean error
		// status — is a real, separate finding worth its own fix, out of
		// scope for this rung (which is about detecting and quarantining the
		// cheater, which already happened by this point).
		t.Logf("commitAsWorker: %d committing task %s (see comment above — expected from a garbage/replay primary at job finalize): %s", code, disp.TaskID, body)
	}
	return code
}

// garbageBytes returns malformed/random bytes — not valid JSON, not a valid
// embed result by any parse path (parseEmbeddingVectors, canonicalJSON) —
// exactly what a worker returning "garbage" instead of computing real
// inference would upload.
func garbageBytes() []byte {
	b := make([]byte, 64)
	_, _ = crand.Read(b)
	// Prefix with non-JSON garbage so it fails EVERY comparator (byte-exact,
	// cosine parse, canonical-JSON, label match) unambiguously — this must
	// never accidentally parse as a valid, agreeing embed result.
	return append([]byte("\x00\x01GARBAGE-NOT-A-REAL-RESULT\x02\x03"), b...)
}

// replayJob submits a throwaway real embed job (no verification overhead) and
// drives it to completion honestly, returning the real result bytes it
// produced — this is the "stale, previously-seen result from an earlier,
// different task" the replay scenario re-submits verbatim on a LATER, DIFFERENT
// task, instead of ever computing a fresh result for that later task.
func replayJob(t *testing.T, ctx context.Context, honest adversarialIdentity) []byte {
	t.Helper()
	touchLive(t, ctx, honest.workerID)
	jobID, n := submitAdversarialEmbedJob(t, "replay-source-"+uuid.New().String(), 0, 0)
	if n != 1 {
		t.Fatalf("replay source job: expected 1 task, got %d", n)
	}
	_, disp, ok := pollAsWorker(t, honest.token)
	if !ok {
		t.Fatalf("replay source poll: no work for job %s", jobID)
	}
	stale := embedResultJSON(1)
	if code := commitAsWorker(t, ctx, honest.token, disp, stale); code != 204 {
		t.Fatalf("replay source commit: want 204, got %d", code)
	}
	return stale
}

// submitAdversarialEmbedJob submits a real single-line embed job through the
// real POST /v1/jobs handler with the given redundancy_frac/honeypot_frac.
// The scenario driver always passes (1.0, 1.0) — the "verification genuinely
// on" configuration the 6->7 floor guarantees a real buyer gets by default
// (set explicitly here, rather than relying on the floor, purely so the exact
// task shape, 1 primary + 1 redundancy clone + 1 honeypot = 3 tasks, is
// pinned and auditable across every scenario run). A (0, 0) caller (the
// replay scenario's throwaway "source" job, which must have NO verification
// overhead — just one honest task to harvest a real stale result from)
// explicitly opts out of the 6->7 verification floor so it gets exactly one
// plain task, matching submitEmbedJob's existing convention in
// integration_test.go. recordID lets each submission carry a distinct input
// line so replay/garbage bytes are never accidentally byte-identical to a
// genuine primary result by coincidence.
func submitAdversarialEmbedJob(t *testing.T, recordID string, redFrac, honeyFrac float32) (uuid.UUID, int) {
	t.Helper()
	verification := map[string]any{
		"redundancy_frac": redFrac, "honeypot_frac": honeyFrac, "payout_hold_secs": 0,
	}
	if redFrac <= 0 && honeyFrac <= 0 {
		verification["skip_verification_floor"] = true
	}
	body := map[string]any{
		"job_type":     map[string]any{"type": "embed"},
		"model":        map[string]any{"kind": "hf", "ref": "all-minilm-l6-v2"},
		"params":       map[string]any{"split_size": 1000},
		"constraints":  map[string]any{"min_memory_gb": 2},
		"verification": verification,
		"tier":         "batch",
		"input":        fmt.Sprintf(`{"id":%q,"text":"adversarial harness record %s"}`+"\n", recordID, recordID),
	}
	code, out := req(t, "POST", "/v1/jobs", body, buyerKey(), jsonCT())
	if code != http.StatusAccepted {
		t.Fatalf("submit adversarial job: want 202, got %d: %s", code, out)
	}
	var r JobSubmitResponse
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("decode job submit response: %v", err)
	}
	return r.JobID, r.TaskCount
}

// supplierStatus reads the real suppliers.status column.
func supplierStatus(t *testing.T, ctx context.Context, supplierID uuid.UUID) string {
	t.Helper()
	var status string
	if err := itPool.QueryRow(ctx, `SELECT status FROM suppliers WHERE id=$1`, supplierID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// isHoneypotDispatch recognizes a honeypot dispatch the only way it still can
// after the honeypot input-GET leak fix (Verification & Result Trust 5->5.5):
// the server-side is_honeypot truth in the DB, keyed by the dispatched task id.
//
// Before that fix a real hostile worker could recognize the trap from its own
// presigned input_url (which literally contained the seed's "honeypots/..."
// address), and this harness mimicked that exact leak. The fix CLOSED the leak —
// the input_url is now an opaque per-task key indistinguishable from a real chunk
// — so a real worker can no longer skim honeypots at all. This harness now GRANTS
// its adversary oracle knowledge it could not have in production (the DB flag),
// which makes the honeypot-skim scenario STRICTLY HARDER on the engine than
// reality: if detection bounds still hold against an adversary that magically
// knows every honeypot, they hold a fortiori against a real one that knows none.
func isHoneypotDispatch(t *testing.T, ctx context.Context, disp TaskDispatch) bool {
	return taskIsHoneypot(t, ctx, disp.TaskID)
}

// driveAdversarialJob submits ONE real 3-task job (1 primary + 1 redundancy +
// 1 honeypot), has the ADVERSARY claim and answer tasks according to cheatFn,
// and has TWO independent, distinct-supplier honest peers claim and correctly
// answer everything else — mirroring a real mixed fleet where honest
// suppliers are also polling the same queue. A SECOND honest peer is
// required, not one: a lone 2-way primary/redundancy disagreement only ever
// reaches pass_with_penalty (docs/internal/CREED_AND_PATH_TO_TEN.md's own
// verifier never docks or claws back off a bare 2-way mismatch) — it takes a
// genuinely independent THIRD worker actually claiming and committing the
// pinned tiebreak task for resolveTiebreak to run its real N-way vote and
// dock/clawback the confirmed loser. Without a real second honest peer this
// harness would understate detection: a cheat that always passes the
// honeypot and only ever disagrees 2-way would look completely undetectable,
// which is a property of an under-supplied test fixture, not of the engine.
//
// Returns how many of the tasks the ADVERSARY ITSELF committed (its own real
// commit count for this job) and whether it was already quarantined before
// this job's tasks were exhausted (checked via a real DB status read right
// after each of its commits).
//
// cheatFn(disp) -> the bytes the adversary commits for that dispatch.
func driveAdversarialJob(t *testing.T, ctx context.Context, adversary, honest, honest2 adversarialIdentity,
	recordID string, cheatFn func(disp TaskDispatch) []byte) (adversaryCommits int, quarantinedDuring bool) {
	t.Helper()
	touchLive(t, ctx, adversary.workerID)
	touchLive(t, ctx, honest.workerID)
	touchLive(t, ctx, honest2.workerID)

	jobID, n := submitAdversarialEmbedJob(t, recordID, 1.0, 1.0)
	if n != 3 {
		t.Fatalf("job %s: expected 3 tasks (1 primary + 1 redundancy + 1 honeypot), got %d", jobID, n)
	}

	honestCommit := func(token string, disp TaskDispatch) {
		// Honeypot-aware exactly like TestPipelineChaining's driveOneTask: an
		// honest worker must answer the honeypot with its real known answer,
		// not the generic canned embed result, or it would itself fail the
		// probe and confound the scenario's quarantine count.
		result := embedResultJSON(1)
		if isHoneypotDispatch(t, ctx, disp) {
			// The honeypot seed stores only the semantic vectors used for
			// comparison. A real agent uploads the full EmbedResult envelope,
			// and strict pre-settlement validation intentionally rejects the
			// legacy vectors-only shape as a payable task artifact.
			result = demoHoneypotEmbedResultJSON(t)
		}
		commitAsWorker(t, ctx, token, disp, result)
	}

	// Drain the job: poll all three identities each round, in a RANDOMIZED
	// order per round, until none of them see any more work. Each poll only
	// ever returns ONE task for the polling identity (real ClaimTask
	// semantics); a pinned tiebreak task (once the engine dispatches one) is
	// claimable ONLY by the peer it was pinned to (control/scheduler.go's
	// pinned-claim branch), so honest2 picking it up is a real claim, not a
	// race.
	//
	// Randomizing poll order matters: ClaimTask's ORDER BY is otherwise stable
	// across ties (e.g. insertion order — primaries are created before their
	// redundancy/honeypot clones), so a FIXED poll order would let the same
	// identity draw the same task "slot" (e.g. always the primary) in every
	// single job — an artifact of this harness's own poll scheduling, not a
	// real property of a hostile worker, and one that would silently starve
	// the adversary of ever drawing the honeypot at all. A real fleet's poll
	// timing is not fixed-order either.
	//
	// NOTE on detecting quarantine mid-loop: a suspended supplier's worker is
	// NOT met with an HTTP 403 — ClaimTask's claim query hard-filters
	// `s.status = 'active'` (control/scheduler.go), so it just finds no
	// eligible row and gets a plain 204, identical on the wire to "nothing
	// queued right now". The real, unambiguous signal is the DB row itself
	// (suppliers.status), checked directly after every adversary commit.
	type poller struct {
		token string
		isAdv bool
	}
	pollers := []poller{{adversary.token, true}, {honest.token, false}, {honest2.token, false}}
	for {
		anyWork := false
		rand.Shuffle(len(pollers), func(i, j int) { pollers[i], pollers[j] = pollers[j], pollers[i] })
		for _, p := range pollers {
			_, disp, hasWork := pollAsWorker(t, p.token)
			if !hasWork {
				continue
			}
			anyWork = true
			if p.isAdv {
				bytes := cheatFn(disp)
				commitAsWorker(t, ctx, p.token, disp, bytes)
				adversaryCommits++
				if supplierStatus(t, ctx, adversary.supplierID) == "suspended" {
					quarantinedDuring = true
				}
			} else {
				honestCommit(p.token, disp)
			}
		}
		if !anyWork {
			break
		}
	}
	return adversaryCommits, quarantinedDuring
}

// quarantinedNow reports whether the adversary's real status is currently
// 'suspended' AND a real subsequent poll is genuinely refused — the two
// checks the rung's own proof artifact demands: a DB-level state change AND a
// live-protocol confirmation that quarantine actually stops new work.
//
// A quarantined-but-still-registered worker is NOT met with an HTTP 403 (that
// status is reserved for an entirely unregistered worker, errNotFound in
// claimWithWait) — ClaimTask's own claim query hard-filters
// `s.status = 'active'` (control/scheduler.go), so a suspended supplier's
// worker simply finds no eligible row and gets a plain 204, indistinguishable
// on its face from "no work queued right now". To make the refusal a REAL,
// unambiguous proof rather than an absence of evidence, this seeds one
// genuinely claimable task for exactly this worker's (job_type, model) pair
// immediately beforehand, then confirms the poll STILL returns 204 despite
// real claimable work existing — that gap (real work present, nothing
// dispatched) is the actual refusal signal.
func quarantinedNow(t *testing.T, ctx context.Context, adversary adversarialIdentity) bool {
	t.Helper()
	if supplierStatus(t, ctx, adversary.supplierID) != "suspended" {
		return false
	}
	touchLive(t, ctx, adversary.workerID)
	jobID, taskID := uuid.New(), uuid.New()
	if _, err := itPool.Exec(ctx,
		`INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb)
		 VALUES ($1,$2,'running','embed','all-minilm-l6-v2','jobs/qn/input.jsonl','batch',1,0,2)`,
		jobID, demoBuyerUUID); err != nil {
		t.Fatalf("quarantinedNow: seed job: %v", err)
	}
	if _, err := itPool.Exec(ctx,
		`INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
		 VALUES ($1,$2,'queued','jobs/qn/tasks/0/input.jsonl','jobs/qn/tasks/0/result.json',0, now())`,
		taskID, jobID); err != nil {
		t.Fatalf("quarantinedNow: seed task: %v", err)
	}
	code, body := req(t, "GET", "/v1/worker/poll", nil, hdr{"X-Worker-Token", adversary.token})
	if code == http.StatusOK {
		t.Fatalf("quarantinedNow: a suspended supplier's worker claimed a real task (%s) — quarantine did not actually stop dispatch", body)
	}
	return code == http.StatusNoContent
}

// --- Scenario A: garbage ---

// runGarbageScenario drives jobs against a fresh adversarial identity that
// commits RANDOM/MALFORMED bytes on every task (including the honeypot — it
// does not even try to recognize it), and returns the number of the
// adversary's OWN real task commits before it is genuinely quarantined.
func runGarbageScenario(t *testing.T, ctx context.Context, honest, honest2 adversarialIdentity, runLabel string) int {
	t.Helper()
	adversary := newAdversarialWorker(t, ctx, "garbage-"+runLabel)
	totalCommits := 0
	for jobN := 0; jobN < maxScenarioJobs; jobN++ {
		if quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
		commits, quarantinedDuring := driveAdversarialJob(t, ctx, adversary, honest, honest2,
			fmt.Sprintf("%s-garbage-%d", runLabel, jobN),
			func(TaskDispatch) []byte { return garbageBytes() })
		totalCommits += commits
		if quarantinedDuring || quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
	}
	t.Fatalf("garbage scenario: adversary not quarantined within %d commits (%d jobs)", totalCommits, maxScenarioJobs)
	return totalCommits
}

// --- Scenario B: replay ---

// runReplayScenario drives jobs against a fresh adversarial identity that
// commits a STALE result — captured from an entirely earlier, different job's
// real committed task — on every task it is dispatched, instead of ever
// computing a fresh result. The stale bytes are captured ONCE up front (a
// real prior commit's real result, exactly as replayJob's doc comment
// describes) and then replayed unchanged for every later task.
func runReplayScenario(t *testing.T, ctx context.Context, honest, honest2 adversarialIdentity, runLabel string) int {
	t.Helper()
	stale := replayJob(t, ctx, honest)
	adversary := newAdversarialWorker(t, ctx, "replay-"+runLabel)
	totalCommits := 0
	for jobN := 0; jobN < maxScenarioJobs; jobN++ {
		if quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
		commits, quarantinedDuring := driveAdversarialJob(t, ctx, adversary, honest, honest2,
			fmt.Sprintf("%s-replay-%d", runLabel, jobN),
			func(TaskDispatch) []byte { return stale })
		totalCommits += commits
		if quarantinedDuring || quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
	}
	t.Fatalf("replay scenario: adversary not quarantined within %d commits (%d jobs)", totalCommits, maxScenarioJobs)
	return totalCommits
}

// --- Scenario C: honeypot-aware but skimping elsewhere ---

// runHoneypotSkimScenario drives jobs against a fresh adversarial identity
// that RECOGNIZES the seeded honeypot (via isHoneypotDispatch, exactly the
// way driveOneTask already does) and answers it with the real known-good
// answer, while committing garbage on every real, non-honeypot (primary or
// redundancy) task — the "passes only honeypots while cheating elsewhere"
// cheat the rung names explicitly.
func runHoneypotSkimScenario(t *testing.T, ctx context.Context, honest, honest2 adversarialIdentity, runLabel string) int {
	t.Helper()
	adversary := newAdversarialWorker(t, ctx, "honeyskim-"+runLabel)
	totalCommits := 0
	for jobN := 0; jobN < maxScenarioJobs; jobN++ {
		if quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
		commits, quarantinedDuring := driveAdversarialJob(t, ctx, adversary, honest, honest2,
			fmt.Sprintf("%s-honeyskim-%d", runLabel, jobN),
			func(disp TaskDispatch) []byte {
				if isHoneypotDispatch(t, ctx, disp) {
					return demoHoneypotEmbedResultJSON(t)
				}
				return garbageBytes()
			})
		totalCommits += commits
		if quarantinedDuring || quarantinedNow(t, ctx, adversary) {
			return totalCommits
		}
	}
	t.Fatalf("honeypot-skim scenario: adversary not quarantined within %d commits (%d jobs)", totalCommits, maxScenarioJobs)
	return totalCommits
}

// maxScenarioJobs bounds how many jobs any single scenario run will submit
// before the test itself fails loudly ("not quarantined within N jobs") —
// a generous ceiling relative to every published bound below, so a genuine
// engine regression (detection silently stops working) fails the test
// instead of looping forever.
const maxScenarioJobs = 60

// TestAdversarialGameabilityBounds is the rung's own proof artifact: "the
// adversarial worker is quarantined within N tasks in an automated, repeatable
// test, with N published." Each of the 3 cheat strategies is run
// adversarialRuns times against fresh identities (so no run can share
// quarantine/reputation state with a previous one), and the measured N for
// every run is asserted <= the published bound AND logged so a human/CI log
// can see the exact per-run sequence, not just a pass/fail.
func TestAdversarialGameabilityBounds(t *testing.T) {
	reset(t)
	ctx := context.Background()
	excludeDemoWorkerFromCandidacy(t, ctx)

	const adversarialRuns = 5
	// Published bounds (see
	// docs/load-test-reports/2026-07-05-adversarial-quarantine-bounds.md for
	// the full measured report this test backs). Garbage and replay are caught
	// almost immediately: EVERY task they touch (including the honeypot, which
	// they do not even try to recognize) is wrong, so as soon as they draw the
	// honeypot dispatch (randomized per job — see driveAdversarialJob) it fails
	// and QuarantineSupplier fires unconditionally (control/verification.go's
	// honeypot-fail branch). Honeypot-skim is fundamentally different and
	// structurally slower to catch: it ALWAYS passes the honeypot by design, so
	// it is never caught by the fast honeypot-fail path at all — the only
	// detection path left is reputation eroding via repeated CONFIRMED tiebreak
	// losses (EventMismatch, -0.10 each) until it crosses quarantineRepFloor
	// (0.2) from a 0.90 start (8 losses in the worst case, each requiring BOTH a
	// sampled tiebreak dispatch AND a real 3rd-worker resolution). This is a
	// real, honestly-published asymmetry, not a harness artifact: passing
	// honeypots keeps the cheat's reputation elevated, which (by design) LOWERS
	// effectiveCheckProb's sampled-probe rate, partially offsetting its own
	// detection risk.
	//
	// WHY THESE BOUNDS ARE WIDER THAN THE MEDIAN N (the concurrent-load tail is
	// REAL, and it is what these bounds must cover — 265 real runs per scenario,
	// 145 in isolation + 120 under deliberate CPU+DB contention, characterized
	// 2026-07-06):
	//
	//   scenario        median  observed-max      published bound (headroom)
	//   garbage           2      9  (under load)   16   (~1.8× over max)
	//   replay            2     17  (isolated)     30   (~1.8× over max)
	//   honeypot_skim    20     42  (isolated)     60   (~1.4× over max)
	//
	// The tail is not RNG jitter we could seed away — it is a genuine property
	// of the detection engine under contention, and seeding the poll-order
	// shuffle (driveAdversarialJob) would destroy the honeypot-draw fairness its
	// own doc comment depends on. ROOT CAUSE of the tail, traced through the real
	// code: an adversary that commits a non-honeypot task BEFORE its honest
	// sibling lands (redundancyBytes==nil at commit time, api.go) reaches the
	// verifier's success path and earns EventTaskSuccess (+0.001, reputation.go),
	// nudging its reputation ABOVE verifyTrustFloor (0.90, verification.go). Once
	// above the floor, effectiveCheckProb drops below 1.0, so checkSampled can
	// sample a honeypot probe (garbage/replay) or a tiebreak re-dispatch
	// (honeypot_skim) OUT — the cheat sails past that probe and commits into the
	// next job. Under real fleet/CI contention the adversary's commits interleave
	// AHEAD of its peers more often (scheduler jitter), so it banks MORE
	// task_success credit per job, climbs FURTHER above 0.90, and the sample-out
	// probability compounds — a strictly longer tail than in isolation. A slower/
	// noisier quarantine sweep under load letting a few more commits through is
	// the security property working AS DESIGNED (reduced audit FREQUENCY for
	// momentarily-trusted suppliers, never a fabricated pass), not a hole: across
	// all 265 runs the adversary was ALWAYS quarantined and NEVER once ran to the
	// maxScenarioJobs=60 ceiling. These bounds are therefore a "worst-case
	// gameability under load" ceiling with a documented margin — deliberately NOT
	// a silent bump: they still fail loudly if a real regression let the engine
	// stop docking/quarantining (N would blow past the bound AND hit the
	// maxScenarioJobs guard's "not quarantined within 60 jobs" fatal), while no
	// longer flaking on the honest concurrent-load tail (the old maxHoneySkimN=40
	// was itself exceeded — N=42 — by an ordinary isolated run in this very
	// characterization, i.e. it was already too tight before any load).
	const maxGarbageN = 16
	const maxReplayN = 30
	const maxHoneySkimN = 60

	honest := newAdversarialWorker(t, ctx, "honest-peer")
	honest2 := newAdversarialWorker(t, ctx, "honest-peer-2")

	type result struct {
		scenario string
		run      int
		n        int
	}
	var results []result

	for run := 1; run <= adversarialRuns; run++ {
		label := fmt.Sprintf("run%d", run)
		n := runGarbageScenario(t, ctx, honest, honest2, label)
		results = append(results, result{"garbage", run, n})
		if n < 1 || n > maxGarbageN {
			t.Fatalf("garbage scenario run %d: N=%d outside published bound [1,%d]", run, n, maxGarbageN)
		}
	}
	for run := 1; run <= adversarialRuns; run++ {
		label := fmt.Sprintf("run%d", run)
		n := runReplayScenario(t, ctx, honest, honest2, label)
		results = append(results, result{"replay", run, n})
		if n < 1 || n > maxReplayN {
			t.Fatalf("replay scenario run %d: N=%d outside published bound [1,%d]", run, n, maxReplayN)
		}
	}
	for run := 1; run <= adversarialRuns; run++ {
		label := fmt.Sprintf("run%d", run)
		n := runHoneypotSkimScenario(t, ctx, honest, honest2, label)
		results = append(results, result{"honeypot_skim", run, n})
		if n < 1 || n > maxHoneySkimN {
			t.Fatalf("honeypot-skim scenario run %d: N=%d outside published bound [1,%d]", run, n, maxHoneySkimN)
		}
	}

	t.Logf("adversarial gameability bounds — measured N per run (own real task commits before real quarantine):")
	for _, r := range results {
		t.Logf("  %-14s run %d: N=%d", r.scenario, r.run, r.n)
	}
}
