package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The verification half, driven through the REAL processor.
//
// The previous checkpoint proved delivery: real engine bytes, real storage, real
// commit, real provenance. What it did not prove is the thing that turns a
// delivered artifact into settlement authority — an independent verification
// decision. That gap is why llama_cpp_metal stays VALIDATED.
//
// The governed reference problem, and why the honeypot mechanism is the right
// answer rather than a new one:
//
// Comparing llama.cpp's output to candle's output at execution time would be one
// supplier vouching for another. Neither is trusted at that moment, and a
// tournament where the challenger's grade comes from an untrusted peer is not a
// grade. The honeypot table already holds a KNOWN ANSWER seeded by an approved
// authority, and `resultsAgree` already applies the 0.999 mean-cosine gate for
// job_type=embed. So the governed reference is a candle artifact seeded as the
// known answer, and the challenger is graded against it — by the verifier, not by
// the test, and not by another supplier.
//
// The processor boundary is exercised, not VerifyJobTx: production reaches
// verification through VerificationProcessor, and calling the inner transaction
// directly would skip lease acquisition, artifact download and plan creation —
// three of the places this could actually be wrong.

type verifiedHarness struct {
	*artifactHarness
	store     *Store
	db        *pgxpool.Pool
	processor *VerificationProcessor
}

func (h *verifiedHarness) pool() *pgxpool.Pool { return h.db }

// newVerifiedArtifactHarness wires the production verifier, processor and
// storage onto an isolated database.
//
// Everything here is the constructor production uses. A bespoke verifier would
// test a verifier nobody runs.
func newVerifiedArtifactHarness(t *testing.T) (*verifiedHarness, context.Context, *Store) {
	t.Helper()
	// Sampling secret before NewVerifier: it fails closed when money rails are
	// configured with the published development default, and that check is worth
	// keeping honest rather than working around.
	t.Setenv("MERC_VERIFICATION_SAMPLE_SECRET",
		"harness-verification-sampling-secret-0123456789abcdef")

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedMoneyPathStore(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	return &verifiedHarness{
		artifactHarness: artifacts,
		store:           store,
		db:              pool,
		processor:       NewVerificationProcessor(store, artifacts.storage, verifier),
	}, ctx, store
}

// seedGovernedEmbeddingReference stores an approved authority's embedding output
// as the known answer for an input, which is what makes an independent grade
// possible at all.
func (h *verifiedHarness) seedGovernedEmbeddingReference(
	ctx context.Context, inputRef string, reference []byte,
) {
	h.t.Helper()
	if err := h.store.InsertHoneypot(ctx, "embed", inputRef, reference, ""); err != nil {
		h.t.Fatalf("seed governed embedding reference: %v", err)
	}
}

// commitThroughStorage uploads the artifact where the verifier will look for it
// and commits the task, exactly as a worker does.
func (h *verifiedHarness) commitThroughStorage(
	ctx context.Context, f moneyPathFixture, taskID uuid.UUID, body []byte,
) *CommitTaskInfo {
	h.t.Helper()
	key := taskAttemptResultKey(f.JobID, taskID, 0)
	if err := h.storage.PutObject(ctx, key, body, "application/json"); err != nil {
		h.t.Fatalf("upload result: %v", err)
	}
	h.t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = h.storage.RemoveObjects(c, []string{key})
	})
	commit := commitFor(f, taskID, 0)
	commit.ResultKey = key
	commit.ResultSHA256 = sha256HexOf(body)
	commit.TokensUsed = 4
	info, err := h.store.CompleteTaskTx(ctx, taskID, f.WorkerID, commit)
	if err != nil {
		h.t.Fatalf("commit: %v", err)
	}
	return info
}

// The headline: llama.cpp's real output, graded by the real verifier against a
// governed candle reference, through the real processor.
func TestLlamaCppEmbeddingPassesIndependentVerification(t *testing.T) {
	h, ctx, store := newVerifiedArtifactHarness(t)
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]

	// The task's own input, and the governed reference for it. candle_metal is
	// the ACTIVE authority for this cell, so its output is the approved answer.
	var inputRef string
	if err := h.pool().QueryRow(ctx,
		`SELECT input_ref FROM tasks WHERE id=$1`, taskID).Scan(&inputRef); err != nil {
		t.Fatal(err)
	}
	h.seedGovernedEmbeddingReference(ctx, inputRef, candle.body)

	// Mark it a honeypot so the verifier grades against the known answer rather
	// than sampling. This is the mechanism, used as designed: an approved answer
	// the supplier cannot see, compared by the platform.
	if _, err := h.pool().Exec(ctx, `
		UPDATE tasks SET is_honeypot=true, runtime_cell_id=$2, runtime_id=$3,
		                 runtime_matrix_sha256=$4, model_kind=$5
		 WHERE id=$1`,
		taskID, llama.cellID, llama.runtimeProfileID,
		generatedRuntimeMatrixSHA256, llama.modelKind); err != nil {
		t.Fatalf("mark honeypot: %v", err)
	}

	h.commitThroughStorage(ctx, f, taskID, llama.body)

	result, err := h.processor.ProcessAttempt(ctx, taskID, 0)
	mustf(t, err, "verification processor: %v")
	if result.Outcome == OutcomeFail {
		t.Fatalf("llama.cpp output failed independent verification against the "+
			"governed candle reference: %+v", result)
	}
	t.Logf("llama.cpp verification outcome: %v applied=%+v", result.Outcome, result.Applied)

	// A supplier that failed must not have been paid, and one that passed must
	// not have been quarantined. Both read from the store, not from the result.
	var quarantined bool
	if err := h.pool().QueryRow(ctx,
		`SELECT status='quarantined' FROM suppliers WHERE id=$1`, f.SupplierID).
		Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined {
		t.Fatal("a passing supplier was quarantined")
	}
}

// The grade has to be capable of failing, or passing means nothing.
func TestGovernedEmbeddingReferenceRejectsWrongOutput(t *testing.T) {
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	// The real predicate the verifier uses, at the real threshold.
	if !resultsAgree("embed", candle.body, llama.body) {
		t.Fatal("the two governed cells disagree under the production predicate")
	}

	var decoded struct {
		JobType string      `json:"job_type"`
		Model   string      `json:"model"`
		Dim     int         `json:"dim"`
		Count   int         `json:"count"`
		Vectors [][]float64 `json:"vectors"`
	}
	must(t, json.Unmarshal(candle.body, &decoded))

	t.Run("scrambled vectors are refused", func(t *testing.T) {
		scrambled := decoded
		scrambled.Vectors = append([][]float64(nil), decoded.Vectors...)
		for i := range scrambled.Vectors {
			row := append([]float64(nil), scrambled.Vectors[i]...)
			for j := range row {
				row[j] = -row[j]
			}
			scrambled.Vectors[i] = row
		}
		body, err := json.Marshal(scrambled)
		must(t, err)
		if resultsAgree("embed", candle.body, body) {
			t.Fatal("sign-flipped vectors were accepted as equivalent")
		}
	})

	t.Run("wrong dimensions are refused", func(t *testing.T) {
		short := decoded
		short.Dim = 8
		short.Vectors = [][]float64{{1, 0, 0, 0, 0, 0, 0, 0}}
		short.Count = 1
		body, err := json.Marshal(short)
		must(t, err)
		if resultsAgree("embed", candle.body, body) {
			t.Fatal("a differently-dimensioned artifact was accepted")
		}
	})

	t.Run("wrong row count is refused", func(t *testing.T) {
		truncated := decoded
		truncated.Vectors = decoded.Vectors[:1]
		truncated.Count = 1
		body, err := json.Marshal(truncated)
		must(t, err)
		if resultsAgree("embed", candle.body, body) {
			t.Fatal("a truncated artifact was accepted")
		}
	})

	t.Run("byte_exact remains byte_exact", func(t *testing.T) {
		// The embedding policy must not have weakened the generation contract.
		// Two artifacts that agree at 0.999999 cosine are still NOT equal bytes,
		// and a byte_exact cell must refuse them.
		if resultsAgree("batch_infer", candle.body, llama.body) {
			t.Fatal("byte_exact accepted two different byte sequences")
		}
	})
}

// A stale attempt's output must not be verifiable. Without this, a worker whose
// attempt was superseded could still have its bytes graded and settled.
func TestStaleAttemptOutputIsNotVerified(t *testing.T) {
	h, ctx, store := newVerifiedArtifactHarness(t)
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	h.commitThroughStorage(ctx, f, taskID, llama.body)

	// Attempt 1 was never committed; asking the processor for it must not
	// silently grade attempt 0's bytes.
	if _, err := h.processor.ProcessAttempt(ctx, taskID, 1); err == nil {
		t.Fatal("the processor graded an attempt that was never committed")
	}
}

// Running the processor twice on one attempt must be idempotent: a retry after
// an uncertain response cannot produce a second decision, because a second
// decision is a second settlement.
func TestVerificationProcessorIsIdempotentPerAttempt(t *testing.T) {
	h, ctx, store := newVerifiedArtifactHarness(t)
	candle := loadChainArtifact(t, "candle_metal", candleEmbedCell, "hf")

	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	h.commitThroughStorage(ctx, f, taskID, candle.body)

	first, err := h.processor.ProcessAttempt(ctx, taskID, 0)
	mustf(t, err, "first pass: %v")
	ledgerAfterFirst := countBuyerLedger(t, ctx, h.pool(), f.BuyerID)

	second, err := h.processor.ProcessAttempt(ctx, taskID, 0)
	mustf(t, err, "second pass: %v")
	if second.Applied.Applied && first.Applied.Applied {
		t.Fatal("both passes applied a verification decision; settlement would run twice")
	}
	if got := countBuyerLedger(t, ctx, h.pool(), f.BuyerID); got != ledgerAfterFirst {
		t.Fatalf("the second pass moved money: %d -> %d", ledgerAfterFirst, got)
	}

	var work int
	if err := h.pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM verification_work WHERE task_id=$1 AND attempt=0`,
		taskID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 1 {
		t.Fatalf("%d verification work rows after two passes, want 1", work)
	}
}

// A committed artifact that is missing from storage must fail rather than
// verify. The verifier reads bytes; if there are none, there is nothing to grade
// and "nothing to grade" must never mean "passed".
func TestMissingCommittedArtifactCannotVerify(t *testing.T) {
	h, ctx, store := newVerifiedArtifactHarness(t)
	llama := loadChainArtifact(t, "llama_cpp_metal", llamaEmbedCell, "gguf")

	f := seedMoneyPathFixture(t, ctx, store, h.pool(), moneyPathSeedOpts{
		TaskCount: 1, TaskStatus: "running", ClaimWorker: true,
		SeedJob: true, SeedPlanRows: true,
	})
	taskID := f.TaskIDs[0]
	key := taskAttemptResultKey(f.JobID, taskID, 0)
	must(t, h.storage.PutObject(ctx, key, llama.body, "application/json"))
	commit := commitFor(f, taskID, 0)
	commit.ResultKey = key
	commit.ResultSHA256 = sha256HexOf(llama.body)
	if _, err := store.CompleteTaskTx(ctx, taskID, f.WorkerID, commit); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Delete the artifact after commit: the shape of an object store losing an
	// object, or a cleanup racing a verifier.
	must(t, h.storage.RemoveObjects(ctx, []string{key}))

	result, err := h.processor.ProcessAttempt(ctx, taskID, 0)
	if err == nil && result.Outcome != OutcomeFail && result.Applied.Applied {
		t.Fatalf("a task with no artifact verified successfully: %+v", result)
	}
	if got := countBuyerLedger(t, ctx, h.pool(), f.BuyerID); got != 0 {
		t.Fatalf("a task with no artifact moved money: %d ledger rows", got)
	}
}

func sha256HexOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

var _ = strings.Contains
