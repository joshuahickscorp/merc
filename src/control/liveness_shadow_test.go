package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Ages exercised by the SQL↔index parity harness. Whole seconds so the
// second-granularity index and timestamptz SQL agree at the edge.
//
//	fresh         0s   live
//	just-inside  44s   live   (last_seen > now-45s)
//	exact-45     45s   DEAD   (SQL is strict >; this is the boundary)
//	just-expired 46s   DEAD
//	old         120s   DEAD
//	never       NULL   DEAD
type liveParityAge struct {
	name string
	age  *int // nil = never seen (NULL last_seen_at)
}

func liveParityAgeSet() []liveParityAge {
	sec := func(n int) *int { return &n }
	return []liveParityAge{
		{"fresh", sec(0)},
		{"just_inside", sec(44)},
		{"exact_45", sec(45)},
		{"just_expired", sec(46)},
		{"old", sec(120)},
		{"never", nil},
	}
}

func (a liveParityAge) wantLive() bool {
	return a.age != nil && *a.age < int(liveDeviceWindowEpochs)
}

// TestLiveDeviceIndexSQLLivenessParity is the G082 shadow-wire deliverable:
// the index-live slot set is IDENTICAL to the SQL last_seen_at > now()-45s
// set at every seeded age, including the exact-45s boundary.
func TestLiveDeviceIndexSQLLivenessParity(t *testing.T) {
	for _, n := range []int{8, 64, 256} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			assertIndexSQLLivenessParity(t, n)
		})
	}
}

func assertIndexSQLLivenessParity(t *testing.T, n int) {
	t.Helper()
	ctx, _, pool := openIsolatedTestStore(t)

	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@parity.test"); err != nil {
		t.Fatalf("supplier: %v", err)
	}

	t0 := time.Now().UTC().Truncate(time.Second)
	ages := liveParityAgeSet()
	rows := make([]liveParityRow, n)
	want := map[uint32]struct{}{}

	// Isolated index we populate the same way the shadow wire does
	// (epoch = last_seen.Unix(), now = t0.Unix()). The store index is
	// also exercised below via Heartbeat for accepted ages.
	idx := NewLiveDeviceIndex(uint32(n + 8))

	for i := 0; i < n; i++ {
		age := ages[i%len(ages)]
		workerID := uuid.New()
		var lastSeen any
		var lastSeenTime *time.Time
		if age.age != nil {
			ls := t0.Add(-time.Duration(*age.age) * time.Second)
			lastSeen = ls
			lastSeenTime = &ls
		}
		var slot int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO workers (id, supplier_id, hw_class, last_seen_at)
			VALUES ($1,$2,'cpu',$3)
			RETURNING device_slot`,
			workerID, supplierID, lastSeen).Scan(&slot); err != nil {
			t.Fatalf("insert worker %d: %v", i, err)
		}
		if slot < 0 || slot > int64(^uint32(0)) {
			t.Fatalf("worker %d device_slot=%d out of uint32", i, slot)
		}
		u := uint32(slot)
		rows[i] = liveParityRow{workerID: workerID, slot: u, age: age, lastSeen: lastSeenTime}
		if lastSeenTime != nil {
			// Same accept rule as resolveHeartbeatObservation / Heartbeat:
			// age>45 is rejected (slot stays DEAD, matching SQL).
			_ = idx.Heartbeat(u, uint32(lastSeenTime.Unix()), uint32(t0.Unix()))
		}
		if age.wantLive() {
			want[u] = struct{}{}
		}
	}

	nowEpoch := uint32(t0.Unix())
	indexLive := slotSet(idx.LiveSlots(nowEpoch))

	sqlLive := map[uint32]struct{}{}
	q, err := pool.Query(ctx, `
		SELECT device_slot FROM workers
		 WHERE last_seen_at IS NOT NULL
		   AND last_seen_at > $1::timestamptz - interval '45 seconds'`, t0)
	if err != nil {
		t.Fatalf("sql live query: %v", err)
	}
	defer q.Close()
	for q.Next() {
		var slot int64
		if err := q.Scan(&slot); err != nil {
			t.Fatalf("scan sql live: %v", err)
		}
		sqlLive[uint32(slot)] = struct{}{}
	}
	if err := q.Err(); err != nil {
		t.Fatalf("sql live rows: %v", err)
	}

	if !slotSetsEqual(indexLive, sqlLive) || !slotSetsEqual(indexLive, want) {
		t.Fatalf("index-live != SQL-live at N=%d t0=%s\n  index-live: %v\n  sql-live:   %v\n  want:       %v\n  %s",
			n, t0.Format(time.RFC3339), sortedSlots(indexLive), sortedSlots(sqlLive),
			sortedSlots(want), formatParityDivergence(rows, indexLive, sqlLive, nowEpoch, idx))
	}

	// Per-slot IsLive must agree with SQL membership, including the
	// exact-45s boundary.
	for _, r := range rows {
		got := idx.IsLive(r.slot, nowEpoch)
		_, sql := sqlLive[r.slot]
		if got != sql {
			t.Fatalf("slot %d age=%s IsLive=%v SQL=%v (boundary must match)",
				r.slot, r.age.name, got, sql)
		}
	}

	t.Logf("N=%d index-live == SQL-live (%d live / %d seeded) ages=%s t0=%s",
		n, len(indexLive), n, formatAgeSummary(rows), t0.Format(time.RFC3339))
}

type liveParityRow struct {
	workerID uuid.UUID
	slot     uint32
	age      liveParityAge
	lastSeen *time.Time
}

func formatParityDivergence(rows []liveParityRow, indexLive, sqlLive map[uint32]struct{}, nowEpoch uint32, idx *LiveDeviceIndex) string {
	var b strings.Builder
	for _, r := range rows {
		_, inIdx := indexLive[r.slot]
		_, inSQL := sqlLive[r.slot]
		if inIdx == inSQL {
			continue
		}
		age := "never"
		if r.age.age != nil {
			age = fmt.Sprintf("%ds", *r.age.age)
		}
		fmt.Fprintf(&b, "  diverge slot=%d worker=%s age=%s(%s) index=%v sql=%v IsLive=%v\n",
			r.slot, r.workerID, r.age.name, age, inIdx, inSQL, idx.IsLive(r.slot, nowEpoch))
	}
	if b.Len() == 0 {
		return "  (set cardinality mismatch without per-slot diffs — unexpected)"
	}
	return b.String()
}

func formatAgeSummary(rows []liveParityRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.age.name]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, counts[n]))
	}
	return strings.Join(parts, ",")
}

func slotSet(slots []uint32) map[uint32]struct{} {
	m := make(map[uint32]struct{}, len(slots))
	for _, s := range slots {
		m[s] = struct{}{}
	}
	return m
}

func slotSetsEqual(a, b map[uint32]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func sortedSlots(m map[uint32]struct{}) []uint32 {
	out := make([]uint32, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestShadowLiveIndexFailClosedUntilHeartbeat: a fresh process has an empty
// index. A shadow selector that used it would see zero live devices until
// heartbeats arrive. Reconstruct-within-45s: one accepted heartbeat makes
// that slot live again. Production selection is not this function.
func TestShadowLiveIndexFailClosedUntilHeartbeat(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "shadow-failclosed-key-32-bytes-min!!!!")
	installSettlementCurrencyForTest(t, "usd")

	nowEpoch := uint32(time.Now().Unix())
	if got := store.shadowSelectLiveSlots(nowEpoch); len(got) != 0 {
		t.Fatalf("process start shadowSelectLiveSlots=%v; fail-closed wants empty", got)
	}

	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	slot, ok := store.lookupOfferSlot(worker.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("seeded worker has no device_slot")
	}

	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	nowEpoch = uint32(time.Now().Unix())
	live := slotSet(store.shadowSelectLiveSlots(nowEpoch))
	if _, ok := live[slot]; !ok {
		t.Fatalf("slot %d not live after heartbeat within 45s; reconstruct failed (live=%v)",
			slot, sortedSlots(live))
	}

	// Process restart: a new Store on the same database must start empty.
	// SQL still has last_seen_at inside the window; the index does not
	// reconstruct from SQL. That is why this lane does not flip selection.
	restarted := NewStore(pool)
	if err := restarted.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt schema: %v", err)
	}
	nowEpoch = uint32(time.Now().Unix())
	if got := restarted.shadowSelectLiveSlots(nowEpoch); len(got) != 0 {
		t.Fatalf("restarted store reconstructed liveness without a heartbeat: %v", got)
	}
	var sqlLive bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers
		   WHERE worker_id=$1 AND last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&sqlLive); err != nil {
		t.Fatal(err)
	}
	if !sqlLive {
		t.Fatal("SQL last_seen is still inside 45s; restart divergence must be index-empty, not SQL-empty")
	}

	if err := restarted.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("reconstruct heartbeat: %v", err)
	}
	nowEpoch = uint32(time.Now().Unix())
	live = slotSet(restarted.shadowSelectLiveSlots(nowEpoch))
	if _, ok := live[slot]; !ok {
		t.Fatalf("slot %d did not reconstruct within 45s after restart heartbeat", slot)
	}
}

// TestHeartbeatRealtimeOfferPopulatesShadowIndex: the production heartbeat
// path writes SQL last_seen_at AND the shadow index. Fresh / just-inside
// match; this does not change who authorize would pick.
func TestHeartbeatRealtimeOfferPopulatesShadowIndex(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "shadow-populate-key-32-bytes-min!!!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)
	slot, ok := store.lookupOfferSlot(worker.WorkerID, profile.RuntimeProfileID)
	if !ok {
		t.Fatal("seeded worker has no device_slot")
	}

	// just-inside: 10s-old observation, well inside 45s.
	obs := time.Now().UTC().Add(-10 * time.Second)
	ms := obs.UnixMilli()
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &ms,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	var sqlLive bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM realtime_worker_offers
		   WHERE worker_id=$1 AND last_seen_at > now() - interval '45 seconds')`,
		worker.WorkerID).Scan(&sqlLive); err != nil {
		t.Fatal(err)
	}
	nowEpoch := uint32(time.Now().Unix())
	indexLive := false
	for _, s := range store.shadowSelectLiveSlots(nowEpoch) {
		if s == slot {
			indexLive = true
			break
		}
	}
	if !sqlLive {
		t.Fatal("SQL offer should be live after a 10s-old heartbeat")
	}
	if !indexLive {
		t.Fatal("shadow index did not take the production heartbeat")
	}

	// Stale observation is rejected: neither SQL nor index may advance.
	beforeSQL := time.Time{}
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers WHERE worker_id=$1`,
		worker.WorkerID).Scan(&beforeSQL); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-realtimeOfferLivenessWindow - 5*time.Second).UnixMilli()
	err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE", ObservedAtUnixMs: &stale,
	})
	if err == nil {
		t.Fatal("stale heartbeat must be rejected")
	}
	var afterSQL time.Time
	if err := pool.QueryRow(ctx, `
		SELECT last_seen_at FROM realtime_worker_offers WHERE worker_id=$1`,
		worker.WorkerID).Scan(&afterSQL); err != nil {
		t.Fatal(err)
	}
	if !afterSQL.Equal(beforeSQL) {
		t.Fatalf("rejected stale write mutated SQL last_seen_at %v → %v", beforeSQL, afterSQL)
	}
}

func TestDeviceSlotAssignedAtEnrolment(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	t.Setenv("MERC_TOKEN_KEY", "device-slot-enroll-key-32-bytes-min!!")

	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@slot.test"); err != nil {
		t.Fatalf("supplier: %v", err)
	}

	// store_workers path (CreateWorkerToken). Isolated DB: first slots are 0,1,2.
	var slots []int64
	for i := 0; i < 3; i++ {
		workerID := uuid.New()
		if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
			t.Fatalf("CreateWorkerToken %d: %v", i, err)
		}
		var slot int64
		if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, workerID).Scan(&slot); err != nil {
			t.Fatalf("read slot %d: %v", i, err)
		}
		slots = append(slots, slot)
	}
	seen := map[int64]bool{}
	for i, s := range slots {
		if s < 0 {
			t.Fatalf("slot %d is negative: %d", i, s)
		}
		if seen[s] {
			t.Fatalf("device_slot %d reused — must be unique", s)
		}
		seen[s] = true
	}
	if slots[1] <= slots[0] || slots[2] <= slots[1] {
		t.Fatalf("slots not monotonic: %v", slots)
	}

	// Re-mint on the same worker must not reassign the slot.
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, workerID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	var second int64
	if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, workerID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("re-enrolment reassigned device_slot %d → %d; slot must be stable", first, second)
	}

	// Device-bound enrolment path (EnrollWorkerTx) also assigns a slot and
	// does not put it on the credential result.
	buyerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		buyerID, buyerID.String()+"@slot.test"); err != nil {
		t.Fatalf("buyer: %v", err)
	}
	out := enrollTestWorker(t, ctx, store, buyerID, newEnrollmentTestKey(t), "http://127.0.0.1:8080", nil)
	var enrollSlot int64
	if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, out.WorkerID).Scan(&enrollSlot); err != nil {
		t.Fatalf("enrolled worker slot: %v", err)
	}
	if enrollSlot < 0 {
		t.Fatalf("enrolled device_slot=%d", enrollSlot)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "device_slot") ||
		strings.Contains(strings.ToLower(string(raw)), "\"slot\"") {
		t.Fatalf("EnrollmentExchangeResult leaked device_slot: %s", raw)
	}

	// Rotation keeps the same worker row and therefore the same slot.
	rotated := enrollTestWorker(t, ctx, store, buyerID, newEnrollmentTestKey(t), "http://127.0.0.1:8080", &out.CredentialID)
	if rotated.WorkerID != out.WorkerID {
		t.Fatalf("rotation minted a new worker %s (want %s)", rotated.WorkerID, out.WorkerID)
	}
	var rotatedSlot int64
	if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, rotated.WorkerID).Scan(&rotatedSlot); err != nil {
		t.Fatal(err)
	}
	if rotatedSlot != enrollSlot {
		t.Fatalf("rotation reassigned device_slot %d → %d", enrollSlot, rotatedSlot)
	}
}

func enrollTestWorker(
	t *testing.T, ctx context.Context, store *Store, buyerID uuid.UUID,
	key enrollmentTestKey, origin string, rotateFrom *uuid.UUID,
) EnrollmentExchangeResult {
	t.Helper()
	raw, _, _, err := parseP256EnrollmentPublicKey(key.encoded)
	mustf(t, err, "parse key: %v")
	requestID := enrollmentRequestID(raw)
	issued, err := store.CreateWorkerEnrollmentCode(ctx, buyerID, EnrollmentCodeIssueInput{
		Audience:               workerEnrollmentAudience,
		DeviceKeyAlgorithm:     workerEnrollmentKeyAlgorithm,
		DevicePublicKey:        key.encoded,
		RotateFromCredentialID: rotateFrom,
	}, enrollmentRequestBinding{
		Version:       workerEnrollmentProtocolVersion,
		ControlOrigin: origin,
		RequestID:     requestID,
	})
	mustf(t, err, "CreateWorkerEnrollmentCode: %v")
	proof := signEnrollmentTestProof(t, key.private, enrollmentExchangeTranscript(
		issued.EnrollmentCode, workerEnrollmentAudience, buyerID, origin, requestID))
	out, err := store.EnrollWorkerTx(ctx, EnrollmentExchangeInput{
		Version:            workerEnrollmentProtocolVersion,
		EnrollmentCode:     issued.EnrollmentCode,
		ControlOrigin:      origin,
		RequestID:          requestID,
		Audience:           workerEnrollmentAudience,
		AccountID:          buyerID,
		DeviceKeyAlgorithm: workerEnrollmentKeyAlgorithm,
		DevicePublicKey:    key.encoded,
		Proof:              proof,
	})
	mustf(t, err, "EnrollWorkerTx: %v")
	return out
}

func TestDeviceSlotIsInternalNotCredential(t *testing.T) {
	// device_slot must not grow an identity/eligibility/money surface.
	typ := reflect.TypeOf(EnrollmentExchangeResult{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "slot") || strings.Contains(name, "device_slot") {
			t.Fatalf("EnrollmentExchangeResult exposes %s — device_slot is not a credential", typ.Field(i).Name)
		}
	}
	auth := reflect.TypeOf(WorkerAuth{})
	for i := 0; i < auth.NumField(); i++ {
		name := strings.ToLower(auth.Field(i).Name)
		if strings.Contains(name, "slot") {
			t.Fatalf("WorkerAuth exposes %s — device_slot must stay internal bookkeeping", auth.Field(i).Name)
		}
	}
}

func TestDeviceSlotSchemaAppliesTwice(t *testing.T) {
	t.Parallel()
	ctx, store, pool := openIsolatedTestStore(t)

	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status) VALUES ($1,$2,'active')`,
		supplierID, supplierID.String()+"@slot-schema.test"); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.New()
	if _, err := store.CreateWorkerToken(ctx, workerID, supplierID); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := pool.QueryRow(ctx, `SELECT device_slot FROM workers WHERE id=$1`, workerID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	mustf(t, store.Migrate(ctx), "second schema apply: %v")

	var after int64
	var notNull, hasDefault, hasUnique, hasRange bool
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT device_slot FROM workers WHERE id=$1),
		  EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name='workers' AND column_name='device_slot'
		       AND is_nullable='NO'
		  ),
		  EXISTS (
		    SELECT 1 FROM information_schema.columns
		     WHERE table_name='workers' AND column_name='device_slot'
		       AND column_default LIKE '%workers_device_slot_seq%'
		  ),
		  EXISTS (
		    SELECT 1 FROM pg_indexes
		     WHERE tablename='workers' AND indexname='workers_device_slot_uidx'
		  ),
		  EXISTS (
		    SELECT 1 FROM pg_constraint
		     WHERE conrelid='workers'::regclass
		       AND conname='workers_device_slot_range'
		  )`, workerID).Scan(&after, &notNull, &hasDefault, &hasUnique, &hasRange); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("second migrate reassigned device_slot %d → %d", before, after)
	}
	if !notNull || !hasDefault || !hasUnique || !hasRange {
		t.Fatalf("device_slot schema incomplete: notNull=%v default=%v unique=%v range=%v",
			notNull, hasDefault, hasUnique, hasRange)
	}
}

func TestShadowIndexNotConsultedForProductionSelection(t *testing.T) {
	// The shadow must be inert: production selection/eligibility/money
	// sources must not name the index, LiveSlots, or device_slot.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	files := []string{
		"scheduler.go",
		"worker_placement.go",
		"market_liquidity.go",
		"service_leases.go",
		"service_lease_data_plane.go",
		"service_market_liquidity.go",
		"realtime_supplier_outcome_stats.go",
		"pricing.go",
		"pricing_decision.go",
		"claim_narrowing.go",
		"store_jobs.go",
	}
	banned := []string{
		"device_slot",
		"LiveSlots(",
		"shadowSelectLiveSlots",
		"shadowIndexHeartbeat",
		"liveIndex",
		"LiveDeviceIndex",
	}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(body)
		for _, needle := range banned {
			if strings.Contains(src, needle) {
				t.Fatalf("%s names %q — production selection must not consult the shadow index", name, needle)
			}
		}
	}

	// HeartbeatRealtimeOffer may populate the index; Authorize must not
	// read it. Check the authorize SQL/body by splitting on the function.
	rt, err := os.ReadFile(filepath.Join(dir, "realtime_store.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(rt)
	idx := strings.Index(text, "func (s *Store) AuthorizeRealtimeContract")
	if idx < 0 {
		t.Fatal("AuthorizeRealtimeContract not found")
	}
	end := strings.Index(text[idx+1:], "\nfunc ")
	if end < 0 {
		end = len(text) - idx
	}
	authBody := text[idx : idx+end]
	for _, needle := range []string{"device_slot", "LiveSlots(", "shadowSelectLiveSlots", "shadowIndexHeartbeat", "liveIndex", "LiveDeviceIndex"} {
		if strings.Contains(authBody, needle) {
			t.Fatalf("AuthorizeRealtimeContract names %q — shadow must be inert", needle)
		}
	}
}

// TestShadowIndexPerOfferGrainNoSiblingRescue is the gate-D proof for the
// offer-grain re-key. One worker with TWO offers (two runtime profiles) that age
// independently. Heartbeat ONE profile; the OTHER offer must read DEAD in the
// index — a live sibling offer must never rescue a stale offer. A worker-grained
// index would (wrongly) mark both live off the single heartbeat; the per-offer
// slot keying is exactly what prevents a stale offer from staying selectable.
func TestShadowIndexPerOfferGrainNoSiblingRescue(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "shadow-pergrain-key-32-bytes-min!!!!")
	installSettlementCurrencyForTest(t, "usd")

	profiles := sortedVLLMProfiles()
	if len(profiles) < 2 {
		// Today the system advertises 1 runtime profile, so a worker has at most
		// one offer and worker-grain == offer-grain (1:1). The sibling-rescue
		// hazard this test guards is a MULTI-profile property (steer S001 §6
		// heterogeneity): it activates automatically once a second profile exists.
		t.Skipf("per-offer-grain sibling-rescue is a multi-profile property; only %d profile advertised (1:1 today)", len(profiles))
	}
	p1, p2 := profiles[0], profiles[1]

	// Worker with an offer for p1 (seed helper), plus a second offer for p2 on
	// the same worker via the same production entry point.
	worker := seedOneRealtimeOffer(t, ctx, store, pool, p1)
	reg2 := RealtimeOfferRegistration{
		RuntimeProfileID: p2.RuntimeProfileID, RuntimeProfileSHA256: p2.ProfileSHA256,
		HWClass: "nvidia_24gb", GPUCount: 1, MemoryGBPerGPU: 24,
		UpstreamBaseURL: "http://127.0.0.1:8811/v1", UpstreamToken: "cx_vllm_liveness_test_token_123456",
		Warmth: "HOT", MaxActiveSequences: 16, AvailableSequences: 8,
		SupplierInputUSDPerMillionTokens: 0.08, SupplierOutputUSDPerMillionTokens: 0.30,
	}
	if err := store.UpsertRealtimeOffer(ctx, worker, reg2); err != nil {
		t.Fatalf("UpsertRealtimeOffer p2: %v", err)
	}

	slot1, ok := store.lookupOfferSlot(worker.WorkerID, p1.RuntimeProfileID)
	if !ok {
		t.Fatal("offer p1 has no offer_slot")
	}
	slot2, ok := store.lookupOfferSlot(worker.WorkerID, p2.RuntimeProfileID)
	if !ok {
		t.Fatal("offer p2 has no offer_slot")
	}
	if slot1 == slot2 {
		t.Fatalf("two offers on one worker share offer_slot %d — grain is worker not offer", slot1)
	}

	// Heartbeat ONLY p1.
	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: p1.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat p1: %v", err)
	}

	nowEpoch := uint32(time.Now().Unix())
	live := slotSet(store.shadowSelectLiveSlots(nowEpoch))
	if _, ok := live[slot1]; !ok {
		t.Fatalf("offer p1 slot %d not live after its own heartbeat", slot1)
	}
	if _, ok := live[slot2]; ok {
		t.Fatalf("offer p2 slot %d is LIVE without its own heartbeat — a live sibling rescued a stale offer (gate D violation)", slot2)
	}
}

func TestShadowIndexConcurrentHeartbeatPopulate(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "shadow-race-key-32-bytes-minimum!!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]

	const n = 32
	workers := make([]WorkerAuth, n)
	slots := make([]uint32, n)
	for i := 0; i < n; i++ {
		workers[i] = seedOneRealtimeOffer(t, ctx, store, pool, profile)
		slot, ok := store.lookupOfferSlot(workers[i].WorkerID, profile.RuntimeProfileID)
		if !ok {
			t.Fatalf("worker %d has no offer_slot", i)
		}
		slots[i] = slot
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(w WorkerAuth) {
			defer wg.Done()
			<-start
			errCh <- store.HeartbeatRealtimeOffer(ctx, w, RealtimeOfferHeartbeat{
				RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
				AvailableSequences: 8, Status: "ACTIVE",
			})
		}(workers[i])
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent heartbeat: %v", err)
		}
	}

	nowEpoch := uint32(time.Now().Unix())
	live := slotSet(store.shadowSelectLiveSlots(nowEpoch))
	var sqlCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM realtime_worker_offers
		 WHERE status='ACTIVE' AND last_seen_at > now() - interval '45 seconds'`).Scan(&sqlCount); err != nil {
		t.Fatal(err)
	}
	if sqlCount != n {
		t.Fatalf("SQL live offers=%d want %d", sqlCount, n)
	}
	if len(live) != n {
		t.Fatalf("index live=%d want %d (slots=%v live=%v)", len(live), n, slots, sortedSlots(live))
	}
	for _, slot := range slots {
		if _, ok := live[slot]; !ok {
			t.Fatalf("slot %d missing from concurrent index-live set", slot)
		}
	}
}

// TestOfferIndexLiveGatesFailClosed exercises the G082 flip's per-offer liveness
// decision (offerIndexLive) directly: it must return true only for a mapped,
// heartbeating offer and fail closed (false) for unmapped, un-heartbeated, and
// fresh-restart states — so the flag-ON selection path can never route to a stale
// or unknown offer.
func TestOfferIndexLiveGatesFailClosed(t *testing.T) {
	ctx, store, pool := openIsolatedTestStore(t)
	store.SetLivenessBatchConfigForTest(livenessBatchConfig{Enabled: false})
	t.Setenv("MERC_TOKEN_KEY", "offerindexlive-key-32-bytes-min!!!!!")
	installSettlementCurrencyForTest(t, "usd")
	profile := sortedVLLMProfiles()[0]
	now := time.Now().UTC()

	// Unknown offer before any seed: fail closed.
	if store.offerIndexLive(uuid.New(), profile.RuntimeProfileID, now) {
		t.Fatal("offerIndexLive true for an unmapped offer — must fail closed")
	}

	worker := seedOneRealtimeOffer(t, ctx, store, pool, profile)

	// Registration is a liveness assertion (last_seen_at=now()) and populates the
	// index, so a just-registered offer is live under flag ON — parity with the SQL
	// last_seen_at predicate, not a heartbeat requirement.
	if !store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("offerIndexLive false right after registration — must match SQL last_seen_at=now()")
	}

	if err := store.HeartbeatRealtimeOffer(ctx, worker, RealtimeOfferHeartbeat{
		RuntimeProfileID: profile.RuntimeProfileID, Warmth: "HOT",
		AvailableSequences: 8, Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("offerIndexLive false right after its own heartbeat — must be live")
	}
	// A different profile on the same worker is a different (unmapped) offer: dead.
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID+"-absent", time.Now().UTC()) {
		t.Fatal("offerIndexLive true for a sibling/unknown profile — must fail closed")
	}
	// Past the 45s window (query far-future now): dead, matching the SQL predicate.
	future := time.Now().UTC().Add(90 * time.Second)
	if store.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, future) {
		t.Fatal("offerIndexLive true 90s after heartbeat — 45s window not enforced")
	}

	// Fresh store on the same DB (restart): index empty, offer reads dead until
	// it re-heartbeats — no selection off a stale durable last_seen_at.
	restarted := NewStore(pool)
	if err := restarted.adoptMigratedSchema(ctx); err != nil {
		t.Fatalf("restart adopt: %v", err)
	}
	if restarted.offerIndexLive(worker.WorkerID, profile.RuntimeProfileID, time.Now().UTC()) {
		t.Fatal("offerIndexLive true on a fresh restart before re-heartbeat — must fail closed")
	}
}

func TestLivenessIndexAuthoritativeFlagDefaultsOff(t *testing.T) {
	if livenessIndexAuthoritative() {
		t.Fatal("MERC_LIVENESS_INDEX_AUTHORITATIVE must default OFF")
	}
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "true")
	if !livenessIndexAuthoritative() {
		t.Fatal("flag not read as true")
	}
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "false")
	if livenessIndexAuthoritative() {
		t.Fatal("flag not read as false")
	}
	t.Setenv("MERC_LIVENESS_INDEX_AUTHORITATIVE", "garbage")
	if livenessIndexAuthoritative() {
		t.Fatal("unparseable flag must be OFF (fail safe)")
	}
}
