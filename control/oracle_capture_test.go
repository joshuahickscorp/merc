//go:build integration

package main

// oracle_capture_test.go — the differential behavioral oracle for the event-horizon
// reconstruction. Snapshots the externally observable DB state of the CURRENT core
// into a canonical, uuid- and clock-normalized JSON bundle, so an aggressive internal
// rewrite can be proven to preserve behavior by byte-equality against committed
// goldens. Reuses the whole integration_test.go harness (TestMain pool itPool/itStore/
// itStorage, req(), reset(), *Key() helpers). The machine-readable contract this
// guards is control/oracle/behavior_matrix.json.
//
// Capture (old core, once):    ORACLE_UPDATE=1 go test -tags integration -run TestOracle...
// Verify (replacement core):   go test -tags integration -run TestOracle...   (no flag)

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ─── domain → capture queries (real table + column names) ────────────────────────
//
// Each query projects one jsonb object per row, money NUMERIC cast ::text (no float
// reformat), and MUST ORDER BY a behavior-deterministic business key (never id) so
// first-seen uuid-ordinal assignment is stable across identical runs.

type captureQuery struct {
	table string
	sql   string
}

var oracleDomains = map[string][]captureQuery{
	"jobs": {{"jobs", `SELECT jsonb_build_object(
		'id',id,'buyer_id',buyer_id,'status',status,'job_type',job_type,'model_ref',model_ref,
		'tier',tier,'estimated_usd',estimated_usd::text,'actual_usd',actual_usd::text,
		'max_usd',max_usd::text,'task_count',task_count,'tasks_done',tasks_done,
		'charge_status',charge_status,'firm_quote',firm_quote,'quote_id',quote_id)
		FROM jobs ORDER BY status,job_type,tier,charge_status,estimated_usd`}},
	"tasks": {{"tasks", `SELECT jsonb_build_object(
		'job_id',job_id,'status',status,'chunk_index',chunk_index,'is_honeypot',is_honeypot,
		'is_redundancy',is_redundancy,'retry_count',retry_count,
		'verification_outcome',verification_outcome,'expected_output_records',expected_output_records,
		'economic_buyer_charge_usd',economic_buyer_charge_usd::text,
		'economic_supplier_payout_usd',economic_supplier_payout_usd::text)
		FROM tasks ORDER BY job_id::text,chunk_index,is_honeypot,is_redundancy,retry_count`}},
	"task_verdicts": {
		{"task_verdicts", `SELECT jsonb_build_object(
		'attempt',attempt,'outcome',outcome,'result_sha256',result_sha256,'decision_sha256',decision_sha256)
		FROM task_verdicts ORDER BY job_id::text,task_id::text,attempt`},
		{"task_verdict_resolutions", `SELECT jsonb_build_object('kind',kind)
		FROM task_verdict_resolutions ORDER BY task_id::text,kind`},
	},
	"verification_events": {{"verification_events", `SELECT jsonb_build_object(
		'kind',kind,'attempt',attempt)
		FROM verification_events ORDER BY job_id::text,COALESCE(task_id::text,''),kind,COALESCE(attempt,0)`}},
	"ledger": {{"ledger_entries", `SELECT jsonb_build_object(
		'kind',kind,'amount_usd',amount_usd::text,'payout_status',payout_status,
		'has_release_at',release_at IS NOT NULL,'has_payout_ref',payout_ref IS NOT NULL)
		FROM ledger_entries ORDER BY COALESCE(task_id::text,''),kind,amount_usd`}},
	"reserve": {
		{"job_economic_plans", `SELECT jsonb_build_object(
		'initial_task_count',initial_task_count,
		'buyer_charge_per_task_usd',buyer_charge_per_task_usd::text,
		'supplier_payout_per_task_usd',supplier_payout_per_task_usd::text,
		'initial_buyer_charge_usd',initial_buyer_charge_usd::text)
		FROM job_economic_plans ORDER BY job_id::text`},
		{"job_economic_reserves", `SELECT jsonb_build_object(
		'reserved_tasks',reserved_tasks,'consumed_tasks',consumed_tasks)
		FROM job_economic_reserves ORDER BY job_id::text`},
	},
	"webhooks": {{"webhooks", `SELECT jsonb_build_object(
		'url',url,'attempts',attempts,'delivered',delivered_at IS NOT NULL)
		FROM webhooks ORDER BY job_id::text,url`}},
}

// ─── normalization ───────────────────────────────────────────────────────────────

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// refKeys hold opaque external identifiers whose raw values are non-deterministic
// (Stripe PI/charge/transfer, operation/fund refs). Their VALUES map through the same
// first-seen ordinal namespace as uuids so cross-table linkage survives while the
// random value is erased.
var refKeys = map[string]bool{
	"payment_intent": true, "charge_id": true, "transfer_ref": true, "stripe_pi": true,
	"collection_payment_intent": true, "operation_key": true, "fund_ref": true,
	"subsidy_authorization_ref": true, "correlation_ref": true, "object_id": true,
	"event_id": true, "payout_ref": true, "quote_id": true,
}

// elidedKeys: volatile free-text columns that carry no behavioral contract. Any
// *_at column is elided by suffix.
var elidedKeys = map[string]bool{"lease_owner": true, "last_error": true, "reason": true}

func elidedKey(k string) bool { return strings.HasSuffix(k, "_at") || elidedKeys[k] }

type normalizer struct {
	seq  int
	seen map[string]string
}

func newNormalizer() *normalizer {
	n := &normalizer{seen: map[string]string{}}
	// Pre-seed shared demo identities to stable friendly ordinals (seed.go).
	n.seen[demoBuyerID] = "buyer:demo"
	n.seen[demoSupplierID] = "supplier:demo"
	n.seen[demoWorkerID] = "worker:demo"
	return n
}

func (n *normalizer) tok(raw string) string {
	if v, ok := n.seen[raw]; ok {
		return v
	}
	id := fmt.Sprintf("id:%d", n.seq)
	n.seq++
	n.seen[raw] = id
	return id
}

// walk normalizes one decoded row: uuid-valued and ref-keyed strings → ordinals,
// elided keys dropped.
func (n *normalizer) walk(key string, v any) any {
	switch t := v.(type) {
	case string:
		if uuidRe.MatchString(t) || refKeys[key] {
			return n.tok(t)
		}
		return t
	case map[string]any:
		// Iterate keys in sorted order so first-seen uuid/ref ordinal assignment is
		// deterministic across captures of identical state (Go map order is random).
		out := map[string]any{}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if elidedKey(k) {
				continue
			}
			out[k] = n.walk(k, t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = n.walk(key, val)
		}
		return out
	default:
		return v // numbers, bools, nil
	}
}

// ─── capture ─────────────────────────────────────────────────────────────────────

// captureObservable snapshots the requested domains into one canonical bundle. The
// uuid/ref ordinal namespace is SHARED across all domains in the call so cross-domain
// foreign keys normalize equally.
func captureObservable(t *testing.T, domains ...string) map[string]any {
	t.Helper()
	ctx := context.Background()
	norm := newNormalizer()
	bundle := map[string]any{}
	for _, d := range domains {
		queries, ok := oracleDomains[d]
		if !ok {
			t.Fatalf("captureObservable: unknown domain %q", d)
		}
		for _, q := range queries {
			rows, err := itPool.Query(ctx, q.sql)
			if err != nil {
				t.Fatalf("capture %s: %v", q.table, err)
			}
			var out []any
			for rows.Next() {
				var raw map[string]any
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					t.Fatalf("capture scan %s: %v", q.table, err)
				}
				out = append(out, norm.walk("", raw))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("capture rows %s: %v", q.table, err)
			}
			if out == nil {
				out = []any{}
			}
			bundle[q.table] = out
		}
	}
	return bundle
}

// oracleCanonicalJSON marshals with sorted keys (encoding/json sorts map keys) and stable
// indentation so goldens diff line-by-line.
func oracleCanonicalJSON(t *testing.T, bundle map[string]any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatalf("oracleCanonicalJSON: %v", err)
	}
	return append(b, '\n')
}

// ─── scenario runner ─────────────────────────────────────────────────────────────

type oracleStep struct {
	// exactly one field is set
	HTTP    *httpStep
	Commit  *commitStep
	Advance *advanceStep
	Sweep   *sweepStep
	SQL     *sqlStep
}

type httpStep struct {
	Method, Path string
	Body         any
	Headers      []hdr
}
type commitStep struct {
	TaskID     uuid.UUID
	ResultKey  string
	Result     []byte
	DurationMS uint64
	TokensUsed uint64
	Headers    []hdr
}
type advanceStep struct {
	MakeDuePayouts, MakeVisibleTasks bool
	Then                             string // optional extra args-free UPDATE
}
type sweepStep struct {
	ReleasePayouts bool
}
type sqlStep struct {
	query string
	args  []any
}

type capturedResponse struct {
	Step   int
	Status int
}

type scenarioRun struct {
	Responses []capturedResponse
	Capture   map[string]any
}

// runScenario executes an ordered scenario and returns the HTTP status sequence plus
// a final capture of the requested domains.
func runScenario(t *testing.T, domains []string, steps []oracleStep) scenarioRun {
	t.Helper()
	ctx := context.Background()
	run := scenarioRun{}
	for i, s := range steps {
		switch {
		case s.HTTP != nil:
			code, _ := req(t, s.HTTP.Method, s.HTTP.Path, s.HTTP.Body, s.HTTP.Headers...)
			run.Responses = append(run.Responses, capturedResponse{i, code})
		case s.Commit != nil:
			c := s.Commit
			if err := itStorage.PutObject(ctx, c.ResultKey, c.Result, "application/json"); err != nil {
				t.Fatalf("step %d put result: %v", i, err)
			}
			commit := TaskCommit{TaskID: c.TaskID, ResultKey: c.ResultKey, DurationMS: c.DurationMS, TokensUsed: c.TokensUsed}
			h := append([]hdr{workerTok(), jsonCT()}, c.Headers...)
			code, _ := req(t, "POST", "/v1/worker/task/"+c.TaskID.String()+"/commit", commit, h...)
			run.Responses = append(run.Responses, capturedResponse{i, code})
		case s.Advance != nil:
			a := s.Advance
			if a.MakeDuePayouts {
				oracleExec(t, `UPDATE ledger_entries SET release_at=now()-interval '1 minute'
					WHERE kind='supplier_credit' AND payout_status='held'`)
			}
			if a.MakeVisibleTasks {
				oracleExec(t, `UPDATE tasks SET visible_at=now()-interval '1 second'
					WHERE status IN ('queued','retrying')`)
			}
			if a.Then != "" {
				oracleExec(t, a.Then)
			}
		case s.Sweep != nil:
			if s.Sweep.ReleasePayouts {
				if err := NewWorkers(itStore, itStorage, stubPayout{}).releasePayouts(ctx); err != nil {
					t.Fatalf("step %d releasePayouts: %v", i, err)
				}
			}
		case s.SQL != nil:
			oracleExec(t, s.SQL.query, s.SQL.args...)
		default:
			t.Fatalf("step %d: empty oracle step", i)
		}
	}
	run.Capture = captureObservable(t, domains...)
	return run
}

func oracleExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := itPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("oracleExec %q: %v", sql, err)
	}
}

// ─── golden pattern ──────────────────────────────────────────────────────────────

// assertGolden diffs a run's normalized bundle against control/oracle/golden/<name>.json.
// Capture the OLD core once with ORACLE_UPDATE=1; the replacement core then asserts
// byte-equality with no flag. The response-status sequence is folded in so route
// contract drift is caught alongside DB state.
func assertGolden(t *testing.T, name string, run scenarioRun) {
	t.Helper()
	sort.SliceStable(run.Responses, func(i, j int) bool { return run.Responses[i].Step < run.Responses[j].Step })
	statuses := make([]int, len(run.Responses))
	for i, r := range run.Responses {
		statuses[i] = r.Status
	}
	full := map[string]any{"response_statuses": statuses, "tables": run.Capture}
	got := oracleCanonicalJSON(t, full)
	path := filepath.Join("oracle", "golden", name+".json")
	if os.Getenv("ORACLE_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("oracle: wrote golden %s (%d bytes)", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("oracle: missing golden %s (capture on the old core with ORACLE_UPDATE=1 first): %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("oracle: %s diverged from golden.\n--- got ---\n%s", name, got)
	}
}
