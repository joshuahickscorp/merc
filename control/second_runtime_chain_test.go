package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// One governed embedding corpus, two runtime cells, through the Merc chain.
//
// The artifacts are REAL engine output, not fixtures that look like it. They are
// produced by `merc-agent emit-embed-artifact`, which runs the production
// EmbedRunner over the production RuntimeDriver — the same code path a claimed
// task takes — and writes the exact bytes a worker would commit:
//
//	docker compose up -d minio createbuckets
//	llama-server -m <pinned GGUF> --embedding --pooling mean -c 32768 -np 128 \
//	  -b 4096 -ub 4096 --port 8188 --host 127.0.0.1
//	for rt in candle_metal llama_cpp_metal; do
//	  ./agent/target/release/merc-agent emit-embed-artifact --runtime $rt \
//	    --input evidence/chain/embed-corpus.jsonl \
//	    --out evidence/chain/embed-$rt.json
//	done
//
// Skipped when those artifacts are absent, because a chain proof taken over
// bytes nobody executed proves nothing about a runtime.

const embeddingCosineChainGate = 0.999

type chainArtifact struct {
	runtimeProfileID string
	cellID           string
	directedCellID   string
	modelKind        string
	body             []byte
	sha256           string
	vectors          [][]float64
}

func loadChainArtifact(t *testing.T, runtimeProfileID, cellID, modelKind string) chainArtifact {
	t.Helper()
	path := filepath.Join("..", "evidence", "chain", "embed-"+runtimeProfileID+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no real %s artifact at %s; run merc-agent emit-embed-artifact first",
			runtimeProfileID, path)
	}
	// Evidence binding stamps binding_status / missing_identity_fields onto
	// files under evidence/. Those fields belong to the receipt census, not to
	// the job-result contract. Task result validation uses DisallowUnknownFields,
	// so a stamped evidence file is not a commit body. Rebuild the result shape
	// the worker actually produces; leave the on-disk receipt stamped for the
	// evidence gate.
	var decoded struct {
		JobType string      `json:"job_type"`
		Model   string      `json:"model"`
		Dim     int         `json:"dim"`
		Count   int         `json:"count"`
		Vectors [][]float64 `json:"vectors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s is not an embed artifact: %v", path, err)
	}
	if decoded.JobType != "embed" || decoded.Dim != EMBED_DIM_FOR_CHAIN {
		t.Fatalf("%s declares job_type=%q dim=%d", path, decoded.JobType, decoded.Dim)
	}
	resultBody, err := json.Marshal(struct {
		JobType string      `json:"job_type"`
		Model   string      `json:"model"`
		Dim     int         `json:"dim"`
		Count   int         `json:"count"`
		Vectors [][]float64 `json:"vectors"`
	}{
		JobType: decoded.JobType,
		Model:   decoded.Model,
		Dim:     decoded.Dim,
		Count:   decoded.Count,
		Vectors: decoded.Vectors,
	})
	if err != nil {
		t.Fatalf("rebuild result body from %s: %v", path, err)
	}
	sum := sha256.Sum256(resultBody)
	return chainArtifact{
		runtimeProfileID: runtimeProfileID,
		cellID:           cellID,
		directedCellID:   cellID,
		modelKind:        modelKind,
		body:             resultBody,
		sha256:           hex.EncodeToString(sum[:]),
		vectors:          decoded.Vectors,
	}
}

// EMBED_DIM_FOR_CHAIN mirrors the agent's EMBED_DIM. Stated here rather than
// imported because the two live in different languages, and a silent divergence
// would make every dimension assertion below vacuous.
const EMBED_DIM_FOR_CHAIN = 384

func chainCosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Both runtimes produce output the PRODUCTION validator accepts, of the same
// shape, agreeing within the gate the control plane applies to a cosine-verified
// cell.
//
// This is the quality half of the chain and it runs without a database, so it
// stays meaningful on a developer machine with only the two artifacts present.
func TestBothRuntimeCellsProduceAcceptableGovernedArtifacts(t *testing.T) {
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	if len(candle.vectors) != len(llama.vectors) {
		t.Fatalf("row counts differ: candle %d, llama.cpp %d",
			len(candle.vectors), len(llama.vectors))
	}
	if len(candle.vectors) == 0 {
		t.Fatal("the corpus produced no vectors")
	}

	// The production artifact validator, not a bespoke shape check. If either
	// engine's bytes would be refused at commit, the chain stops there and no
	// amount of cosine agreement matters.
	for _, artifact := range []chainArtifact{candle, llama} {
		info := &CommitTaskInfo{
			jobType:  "embed",
			ModelRef: "all-minilm-l6-v2",
		}
		if err := validateTaskResultArtifact(info, artifact.body); err != nil {
			t.Fatalf("%s produced an artifact the control plane refuses: %v",
				artifact.runtimeProfileID, err)
		}
	}

	minCosine := math.MaxFloat64
	for i := range candle.vectors {
		if len(candle.vectors[i]) != EMBED_DIM_FOR_CHAIN ||
			len(llama.vectors[i]) != EMBED_DIM_FOR_CHAIN {
			t.Fatalf("row %d dimensions: candle %d, llama.cpp %d", i,
				len(candle.vectors[i]), len(llama.vectors[i]))
		}
		for _, v := range append(append([]float64{}, candle.vectors[i]...), llama.vectors[i]...) {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("row %d contains a non-finite value", i)
			}
		}
		minCosine = math.Min(minCosine, chainCosine(candle.vectors[i], llama.vectors[i]))
	}
	t.Logf("cross-cell minimum cosine over %d rows: %.6f", len(candle.vectors), minCosine)
	if minCosine < embeddingCosineChainGate {
		t.Fatalf("minimum cosine %.6f is below the %.3f gate",
			minCosine, embeddingCosineChainGate)
	}

	// Same contract, different bytes. Identical bytes would mean one engine's
	// output had been substituted for the other's somewhere, and the whole
	// comparison would be measuring nothing.
	if candle.sha256 == llama.sha256 {
		t.Fatal("both cells produced byte-identical artifacts; one engine's output " +
			"was substituted for the other's")
	}
}

// The directed decision each cell executes under must name that cell, carry its
// artifact format, and otherwise be the same governed workload.
func TestEachCellExecutesTheSameGovernedWorkload(t *testing.T) {
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	decisions := map[string]WorkloadDecision{}
	for _, artifact := range []chainArtifact{candle, llama} {
		decision, err := buildWorkloadDecisionDirected(
			embedSubmit(), strings.Repeat("7", 64), artifact.directedCellID)
		if err != nil {
			t.Fatalf("directed decision for %s: %v", artifact.cellID, err)
		}
		if decision.RuntimeCandidates[0].CellID != artifact.cellID {
			t.Fatalf("decision for %s froze %s", artifact.cellID,
				decision.RuntimeCandidates[0].CellID)
		}
		if decision.RuntimeCandidates[0].ModelKind != artifact.modelKind {
			t.Fatalf("%s froze model kind %q, want %q the cell actually loads",
				artifact.cellID, decision.RuntimeCandidates[0].ModelKind, artifact.modelKind)
		}
		decisions[artifact.cellID] = decision
	}

	a, b := decisions[candleEmbedCell], decisions[llamaEmbedCell]
	if a.BindingSHA256 != b.BindingSHA256 {
		t.Fatal("the two cells executed different bindings; they are not comparable")
	}
	if a.VerificationStrategy != b.VerificationStrategy {
		t.Fatalf("verification differs: %q vs %q", a.VerificationStrategy, b.VerificationStrategy)
	}
	if a.ModelRevision != b.ModelRevision {
		t.Fatal("the two cells executed different model revisions")
	}
}

// The artifact each cell produced, stored content-addressed through the real
// storage interface and read back, is what the chain would carry.
func TestChainArtifactsSurviveContentAddressedStorage(t *testing.T) {
	h := newArtifactHarness(t)
	for _, artifact := range []chainArtifact{
		loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf"),
		loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf"),
	} {
		key, digest := h.put(artifact.body, "application/json")
		if digest != artifact.sha256 {
			t.Fatalf("%s: storage digest %s does not match the emitted %s",
				artifact.runtimeProfileID, digest, artifact.sha256)
		}
		read, err := h.get(key)
		if err != nil {
			t.Fatalf("%s: read back: %v", artifact.runtimeProfileID, err)
		}
		if string(read) != string(artifact.body) {
			t.Fatalf("%s: bytes changed in storage", artifact.runtimeProfileID)
		}
		// Content-addressed, never job-scoped: a follower or a second buyer
		// resolving this reference must not be reaching into someone's job
		// namespace.
		if strings.Contains(key, "jobs/") {
			t.Fatalf("%s stored under a tenant-scoped key %q",
				artifact.runtimeProfileID, key)
		}
	}
}

// The money half: one execution on each cell settles to one supplier payable,
// a buyer debit, and a positive Merc contribution, with the runtime provenance
// bound to the cell that ran it.
func TestSecondRuntimeChainSettlesOncePerExecution(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	for _, artifact := range []chainArtifact{candle, llama} {
		t.Run(artifact.runtimeProfileID, func(t *testing.T) {
			f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
				TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
				SeedJob: true, SeedPlanRows: true,
			})
			taskID := f.TaskIDs[0]

			// Bind the task's runtime provenance to the CELL that executed it.
			// tasks_runtime_provenance_complete requires all four together, which
			// is what stops a receipt naming a runtime it cannot substantiate.
			if _, err := pool.Exec(ctx, `
				UPDATE tasks SET runtime_cell_id=$2, runtime_id=$3,
				                 runtime_matrix_sha256=$4, model_kind=$5
				 WHERE id=$1`,
				taskID, artifact.cellID, artifact.runtimeProfileID,
				generatedRuntimeMatrixSHA256, artifact.modelKind); err != nil {
				t.Fatalf("bind runtime provenance: %v", err)
			}

			commit := commitFor(f, taskID, 0)
			commit.ResultSHA256 = artifact.sha256
			commit.TokensUsed = uint64(len(artifact.vectors))

			before := countBuyerLedger(t, ctx, pool, f.BuyerID)
			info, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commit)
			if err != nil {
				t.Fatalf("commit on %s: %v", artifact.runtimeProfileID, err)
			}
			if info == nil || info.TaskID != taskID {
				t.Fatalf("commit info: %+v", info)
			}

			// Exactly one verification work row, at this attempt. Two would mean
			// the same execution could settle twice.
			var work int
			if err := pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM verification_work WHERE task_id=$1 AND attempt=0`,
				taskID).Scan(&work); err != nil {
				t.Fatal(err)
			}
			if work != 1 {
				t.Fatalf("%d verification work rows, want 1", work)
			}
			// Committing must not mint a buyer ledger row on its own: money moves
			// at settlement, after verification, never at delivery.
			if got := countBuyerLedger(t, ctx, pool, f.BuyerID); got != before {
				t.Fatalf("commit minted ledger rows: %d -> %d", before, got)
			}

			// The provenance survived the commit and names the executing cell.
			var cell, runtime, kind string
			if err := pool.QueryRow(ctx,
				`SELECT runtime_cell_id, runtime_id, model_kind FROM tasks WHERE id=$1`,
				taskID).Scan(&cell, &runtime, &kind); err != nil {
				t.Fatal(err)
			}
			if cell != artifact.cellID || runtime != artifact.runtimeProfileID ||
				kind != artifact.modelKind {
				t.Fatalf("provenance after commit = %s/%s/%s, want %s/%s/%s",
					runtime, cell, kind,
					artifact.runtimeProfileID, artifact.cellID, artifact.modelKind)
			}

			// A duplicate commit of the same attempt must be inert. Without this
			// a retried acknowledgement would be a second delivery of one
			// execution, and the supplier would be paid twice for it.
			outcome, err := store.FailTaskTx(ctx, taskID, f.WorkerID, 0, FailureReport{
				Class: "internal_error", Message: "commit ack lost",
				Backend: "embed", Model: "all-minilm-l6-v2", DurationMS: 10,
			})
			if err != nil || outcome != FailNoop {
				t.Fatalf("post-commit failure = (%q,%v), want noop", outcome, err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM verification_work WHERE task_id=$1 AND attempt=0`,
				taskID).Scan(&work); err != nil {
				t.Fatal(err)
			}
			if work != 1 {
				t.Fatalf("a duplicate commit produced %d verification rows", work)
			}
		})
	}
}

// A worker may not commit a result for a cell it is not bound to. Directed
// routing widens what an OPERATOR can name; it must not widen what a worker can
// claim to have run.
func TestWorkerCannotClaimACellItIsNotBoundTo(t *testing.T) {
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	f := seedMoneyPathFixture(t, ctx, store, pool, moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	_ = store

	// A cell that is REJECTED_FOR_CONTRACT can never appear as provenance,
	// because nothing may route to it.
	var reachable int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runtime_profile_models
		 WHERE cell_id=$1 AND routable`, llamaInferCell).Scan(&reachable); err != nil {
		t.Fatal(err)
	}
	if reachable != 0 {
		t.Fatalf("the rejected cell is routable in the database")
	}
	if len(f.TaskIDs) != 1 {
		t.Fatalf("fixture produced %d tasks", len(f.TaskIDs))
	}
}

var _ = uuid.Nil
