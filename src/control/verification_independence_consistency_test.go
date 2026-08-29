package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Push, pull, and StartTask must enforce one independence rule: every
// prior executor and current holder of (job_id, COALESCE(chunk_index,0))
// is excluded. A single raw vote is not a redundancy match.

func TestIndependencePriorSetIsSharedAcrossPushPullAndTiebreak(t *testing.T) {
	for _, file := range []string{"scheduler.go", "store_tasks.go", "verification_lifecycle.go"} {
		raw, err := os.ReadFile(file)
		must(t, err)
		if !strings.Contains(string(raw), "chunkIndependencePriorsSQL(") {
			t.Errorf("%s does not call chunkIndependencePriorsSQL", file)
		}
	}
	query := ClaimTaskSQL("t.claimed_by IS NULL")
	for _, required := range []string{
		"prior.claimed_by",
		"holders.supplier_id",
		"prior.execution_worker_id",
		"FROM task_execution_history history",
		"executed.worker_id=ej.claim_worker_id",
		"executed.supplier_id=ej.claim_supplier_id",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("ClaimTaskSQL lost prior-set fragment %q after extraction", required)
		}
	}
	push, err := os.ReadFile("store_tasks.go")
	must(t, err)
	if strings.Contains(string(push), "nw.id IS DISTINCT FROM anchor.execution_worker_id") {
		t.Fatal("push eligibility still excludes only the anchor worker")
	}
}

func TestRedundancyExecutorCannotBePinnedAsThirdOpinion(t *testing.T) {
	ctx, store, pool, f, primaryID := seedIndependencePushJob(t)
	insertChunkSibling(t, ctx, pool, f, primaryID, f.OtherWorkerID, f.OtherSupplierID, true)

	before := readDynamicObligationSnapshot(t, ctx, pool, f.JobID, primaryID, 0)
	_, err := store.InsertTiebreakTask(ctx, f.JobID, primaryID, f.OtherWorkerID, "money/input", 0)
	if !errors.Is(err, ErrNoSupply) {
		t.Fatalf("pinning the redundancy executor as the third opinion error=%v, want ErrNoSupply", err)
	}
	after := readDynamicObligationSnapshot(t, ctx, pool, f.JobID, primaryID, 0)
	if after != before {
		t.Fatalf("refused pin consumed reserve or created a task: before=%+v after=%+v", before, after)
	}
	if after.TiebreakRows != 0 {
		t.Fatalf("redundancy executor was pinned as a tiebreak: %+v", after)
	}
}

func TestIndependentPeerCanStillBePinnedAsThirdOpinion(t *testing.T) {
	ctx, store, pool, f, primaryID := seedIndependencePushJob(t)
	insertChunkSibling(t, ctx, pool, f, primaryID, f.OtherWorkerID, f.OtherSupplierID, true)
	peer := addIndependencePeerWorker(t, ctx, pool)

	id, err := store.InsertTiebreakTask(ctx, f.JobID, primaryID, peer.workerID, "money/input", 0)
	mustf(t, err, "independent peer must remain pinnable as the third opinion: %v")
	if id == uuid.Nil {
		t.Fatal("independent pin returned a nil task id")
	}
	var claimedBy uuid.UUID
	var isRedundancy bool
	var hedgedFrom uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT claimed_by, COALESCE(is_redundancy,false), hedged_from
		  FROM tasks WHERE id=$1`, id).Scan(&claimedBy, &isRedundancy, &hedgedFrom); err != nil {
		t.Fatal(err)
	}
	if claimedBy != peer.workerID || !isRedundancy || hedgedFrom != primaryID {
		t.Fatalf("pinned tiebreak claimed_by=%s redundancy=%v hedged_from=%s, want peer %s redundancy from %s",
			claimedBy, isRedundancy, hedgedFrom, peer.workerID, primaryID)
	}
}

func TestPushPinExcludesClaimedButUnexecutedRedundancyHolder(t *testing.T) {
	ctx, store, pool, f, primaryID := seedIndependencePushJob(t)
	insertChunkSibling(t, ctx, pool, f, primaryID, f.OtherWorkerID, f.OtherSupplierID, false)

	_, err := store.InsertTiebreakTask(ctx, f.JobID, primaryID, f.OtherWorkerID, "money/input", 0)
	if !errors.Is(err, ErrNoSupply) {
		t.Fatalf("pinning a worker that merely holds the redundancy copy error=%v, want ErrNoSupply", err)
	}
}

func TestTiebreakStartExcludesClaimedButUnexecutedSibling(t *testing.T) {
	ctx, store, pool, f, tiebreakID := seedDynamicTiebreakStartFixture(t)
	insertChunkSibling(t, ctx, pool, f, f.TaskIDs[0], f.OtherWorkerID, f.OtherSupplierID, false)

	before := dynamicTiebreakSnapshot(t, ctx, pool, tiebreakID)
	if err := store.StartTask(ctx, tiebreakID, f.OtherWorkerID, 0); !errors.Is(err, errNotFound) {
		t.Fatalf("StartTask for a peer merely holding a sibling error=%v, want errNotFound", err)
	}
	if got := dynamicTiebreakSnapshot(t, ctx, pool, tiebreakID); got != before {
		t.Fatalf("refused start mutated the pinned tiebreak:\nbefore=%+v\nafter=%+v", before, got)
	}
}

func TestSingleRawVoteDoesNotRecordRedundancyMatch(t *testing.T) {
	supplier := uuid.New()
	taskID := uuid.New()
	store, info := newIndependenceVoteFixture(supplier, taskID)
	store.chunkResults = []ChunkResult{{
		TaskID: taskID, SupplierID: supplier,
		Engine: info.engine, BuildHash: info.buildHash, BuildIdentityPolicy: info.buildIdentityPolicy,
	}}

	outcome, err := (&Verifier{store: store, storage: &Storage{}}).
		verifyTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("result"), []byte("peer"))
	if !errors.Is(err, errNoIndependentSupplier) {
		t.Fatalf("single raw vote error=%v, want NO_INDEPENDENT_SUPPLIER", err)
	}
	if outcome != OutcomeFail {
		t.Fatalf("single raw vote outcome=%q, want %q", outcome, OutcomeFail)
	}
	assertNoRedundancyMatch(t, store)
}

func TestTwoSameSupplierVotesStillFailClosed(t *testing.T) {
	supplier := uuid.New()
	taskID := uuid.New()
	store, info := newIndependenceVoteFixture(supplier, taskID)
	store.chunkResults = []ChunkResult{
		{
			TaskID: taskID, SupplierID: supplier,
			Engine: info.engine, BuildHash: info.buildHash, BuildIdentityPolicy: info.buildIdentityPolicy,
		},
		sameClassPeerVote(info, supplier),
	}

	outcome, err := (&Verifier{store: store, storage: &Storage{}}).
		verifyTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("result"), []byte("result"))
	if !errors.Is(err, errNoIndependentSupplier) {
		t.Fatalf("two same-supplier votes error=%v, want NO_INDEPENDENT_SUPPLIER", err)
	}
	if outcome != OutcomeFail {
		t.Fatalf("two same-supplier votes outcome=%q, want %q", outcome, OutcomeFail)
	}
	assertNoRedundancyMatch(t, store)
	if !containsString(store.events, "redundancy_same_supplier") {
		t.Fatalf("same-supplier fail-closed events=%v, want redundancy_same_supplier", store.events)
	}
}

func TestTwoIndependentVotesStillRecordRedundancyMatch(t *testing.T) {
	supplier := uuid.New()
	taskID := uuid.New()
	store, info := newIndependenceVoteFixture(supplier, taskID)
	store.chunkResults = []ChunkResult{
		{
			TaskID: taskID, SupplierID: supplier,
			Engine: info.engine, BuildHash: info.buildHash, BuildIdentityPolicy: info.buildIdentityPolicy,
		},
		sameClassPeerVote(info, uuid.New()),
	}

	outcome, err := (&Verifier{store: store, storage: &Storage{}}).
		verifyTaskResult(context.Background(), info, TaskCommit{TaskID: taskID}, []byte("result"), []byte("result"))
	mustf(t, err, "two independent votes: %v")
	if outcome != OutcomePass {
		t.Fatalf("two independent votes outcome=%q, want %q", outcome, OutcomePass)
	}
	if !containsString(store.events, "redundancy_match") {
		t.Fatalf("independent match events=%v, want redundancy_match", store.events)
	}
	if containsString(store.events, "NO_INDEPENDENT_SUPPLIER") || containsString(store.events, "redundancy_same_supplier") {
		t.Fatalf("independent match was treated as fail-closed: %v", store.events)
	}
}

func TestIndependentRedundancyMatchRejectsFewerThanTwoSuppliers(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	vote := func(id uuid.UUID) chunkVote {
		return chunkVote{supplierID: id, taskID: uuid.New(), bytes: []byte("ok")}
	}
	if independentRedundancyMatch(nil) || independentRedundancyMatch([]chunkVote{vote(s1)}) {
		t.Fatal("fewer than two votes must not be a redundancy match")
	}
	if independentRedundancyMatch([]chunkVote{vote(s1), vote(s1)}) {
		t.Fatal("two votes from one supplier must not be a redundancy match")
	}
	if !independentRedundancyMatch([]chunkVote{vote(s1), vote(s2)}) {
		t.Fatal("two independent suppliers must still match")
	}
}

func seedIndependencePushJob(t *testing.T) (context.Context, *Store, *pgxpool.Pool, moneyPathFixture, uuid.UUID) {
	t.Helper()
	ctx, store, pool := openMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true, SeedJob: true, SeedPlanRows: true,
	})
	primaryID := f.TaskIDs[0]
	if _, err := pool.Exec(ctx,
		`UPDATE tasks SET claimed_at=now(),started_at=now() WHERE id=$1`,
		primaryID); err != nil {
		t.Fatalf("stamp anchor execution: %v", err)
	}
	return ctx, store, pool, f, primaryID
}

func insertChunkSibling(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, f moneyPathFixture,
	primaryID, workerID, supplierID uuid.UUID, executed bool,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	resultKey := taskAttemptResultKey(f.JobID, id, 0)
	var execWorker, execSupplier, execHW, execEngine, execBuild, execPolicy any
	status := "running"
	if executed {
		execWorker, execSupplier = workerID, supplierID
		execHW, execEngine, execBuild = "apple_silicon_max", "candle", "deadbeefdeadbeef"
		execPolicy = currentEngineBuildIdentityPolicy
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks
		  (id,job_id,status,is_redundancy,input_ref,result_key,chunk_index,
		   claimed_by,worker_id,started_at,
		   execution_worker_id,execution_supplier_id,
		   execution_hw_class,execution_engine,execution_build_hash,execution_build_identity_policy,
		   economic_buyer_charge_usd,economic_supplier_payout_usd)
		VALUES ($1,$2,$3,true,'money/input',$4,0,
		        $5,$5,now(),
		        $6,$7,$8,$9,$10,$11,
		        $12,$13)`,
		id, f.JobID, status, resultKey,
		workerID, execWorker, execSupplier, execHW, execEngine, execBuild, execPolicy,
		f.Plan.BuyerChargePerTaskUSD, f.Plan.SupplierPayoutPerTaskUSD); err != nil {
		t.Fatalf("insert chunk sibling: %v", err)
	}
	_ = primaryID
	return id
}

type independencePeerWorker struct {
	supplierID, workerID uuid.UUID
}

func addIndependencePeerWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool) independencePeerWorker {
	t.Helper()
	peer := independencePeerWorker{supplierID: uuid.New(), workerID: uuid.New()}
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,status,reputation,completed_tasks)
		VALUES ($1,$2,'active',0.95,100)`,
		peer.supplierID, "indep-sup-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert independent supplier: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workers (id,supplier_id,hw_class,hardware_identity,memory_gb,effective_memory_gb,
		                     last_seen_at,throttled,min_payout_usd_hr,engine,build_hash,build_identity_policy)
		VALUES ($1,$2,'apple_silicon_max',$3,64,64,now(),false,0.10,'candle','deadbeefdeadbeef',$4)`,
		peer.workerID, peer.supplierID, testOnlyHardwareIdentity, currentEngineBuildIdentityPolicy); err != nil {
		t.Fatalf("insert independent worker: %v", err)
	}
	bindWorkerToGovernedProfile(t, pool, ctx, peer.workerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_authorized_capabilities
		  (worker_id,cell_id,runtime_id,job_type,model_ref,model_kind,matrix_sha256)
		VALUES ($1,'cell','rt','embed','all-minilm-l6-v2','hf',$2)`,
		peer.workerID, generatedRuntimeMatrixSHA256); err != nil {
		t.Fatalf("insert independent capability: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `UPDATE workers SET last_seen_at=now()-interval '10 minutes' WHERE id=$1`, peer.workerID)
		_, _ = pool.Exec(c, `DELETE FROM worker_authorized_capabilities WHERE worker_id=$1`, peer.workerID)
		_, _ = pool.Exec(c, `DELETE FROM workers WHERE id=$1`, peer.workerID)
		_, _ = pool.Exec(c, `DELETE FROM suppliers WHERE id=$1`, peer.supplierID)
	})
	return peer
}

type independenceRecordingStore struct {
	chunkResults []ChunkResult
	events       []string
	reputation   []ReputationEvent
}

func (s *independenceRecordingStore) GetHoneypotAnswer(context.Context, string, string) ([]byte, string, error) {
	return nil, "", nil
}
func (s *independenceRecordingStore) CandidateWorkers(context.Context, string, string, float32) ([]MatchWorker, error) {
	return nil, nil
}
func (s *independenceRecordingStore) ChunkResults(context.Context, uuid.UUID, int) ([]ChunkResult, error) {
	return s.chunkResults, nil
}
func (s *independenceRecordingStore) TiebreakExists(context.Context, uuid.UUID, int) (bool, error) {
	return false, nil
}
func (s *independenceRecordingStore) SelectRedundancyPeerExcluding(context.Context, uuid.UUID, uuid.UUID, string, string, float32, uuid.UUID, []uuid.UUID, []uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, ErrNoSupply
}
func (s *independenceRecordingStore) DockReputation(_ context.Context, _ uuid.UUID, ev ReputationEvent) error {
	s.reputation = append(s.reputation, ev)
	return nil
}
func (s *independenceRecordingStore) RecordVerificationEvent(_ context.Context, _, _, _ uuid.UUID, kind string) error {
	s.events = append(s.events, kind)
	return nil
}
func (s *independenceRecordingStore) ClawbackTaskCredit(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (s *independenceRecordingStore) QuarantineSupplier(context.Context, uuid.UUID) error {
	return nil
}
func (s *independenceRecordingStore) RequeueTask(context.Context, uuid.UUID) error { return nil }
func (s *independenceRecordingStore) InsertTiebreakTask(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, int) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func newIndependenceVoteFixture(supplier, taskID uuid.UUID) (*independenceRecordingStore, *CommitTaskInfo) {
	info := &CommitTaskInfo{
		TaskID:                  taskID,
		JobID:                   uuid.New(),
		SupplierID:              supplier,
		ResultSHA256:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		jobType:                 "batch_infer",
		engine:                  "candle",
		buildHash:               "deadbeefdeadbeef",
		buildIdentityPolicy:     currentEngineBuildIdentityPolicy,
		peerSupplierID:          uuid.New(),
		peerEngine:              "candle",
		peerBuildHash:           "deadbeefdeadbeef",
		peerBuildIdentityPolicy: currentEngineBuildIdentityPolicy,
	}
	return &independenceRecordingStore{}, info
}

func sameClassPeerVote(info *CommitTaskInfo, supplier uuid.UUID) ChunkResult {
	return ChunkResult{
		TaskID:              uuid.New(),
		SupplierID:          supplier,
		Engine:              info.engine,
		BuildHash:           info.buildHash,
		BuildIdentityPolicy: info.buildIdentityPolicy,
		Artifact:            &VerificationArtifact{Key: "peer", SHA256: info.ResultSHA256, Bytes: 1},
	}
}

func assertNoRedundancyMatch(t *testing.T, store *independenceRecordingStore) {
	t.Helper()
	if containsString(store.events, "redundancy_match") {
		t.Fatalf("recorded redundancy_match on a non-independent vote: %v", store.events)
	}
	for _, ev := range store.reputation {
		if ev == EventRedundancyMatch {
			t.Fatalf("credited EventRedundancyMatch on a non-independent vote: %v", store.reputation)
		}
	}
	if !containsString(store.events, "NO_INDEPENDENT_SUPPLIER") {
		t.Fatalf("events=%v, want NO_INDEPENDENT_SUPPLIER", store.events)
	}
}
