package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func loraEvaluationProjectTarArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	declaration := loraProjectDeclarationFixture()
	assets := map[string]string{
		"schema.json":    `{"version":"MERC_LORA_DATASET_SCHEMA_V1","fields":{"input":"string","target":"string"},"required":["input","target"]}`,
		"train.jsonl":    "{\"input\":\"first prompt\",\"target\":\"first completion\"}\n{\"input\":\"second prompt\",\"target\":\"second completion\"}\n",
		"held-out.jsonl": "{\"input\":\"held prompt\",\"target\":\"held completion\"}\n",
		"training.py":    "lora training with an independent held-out evaluator\n",
	}
	for path, contents := range assets {
		digest := sha256.Sum256([]byte(contents))
		switch path {
		case "schema.json":
			declaration.Steps[0].LoRA.DatasetSchema.SHA256 = hex.EncodeToString(digest[:])
		case "train.jsonl":
			declaration.Steps[0].LoRA.TrainingSet.SHA256 = hex.EncodeToString(digest[:])
		case "held-out.jsonl":
			declaration.Steps[0].LoRA.HeldOutSet.SHA256 = hex.EncodeToString(digest[:])
		}
	}
	declarationRaw, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	assets[projectDeclarationName] = string(declarationRaw)
	return projectTarArchive(t, assets), declaration.Steps[0].LoRA.HeldOutSet.SHA256
}

func TestProjectLoRAEvaluationRoutePersistsIndependentOutcomeEvidence(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openPayoutTestStore(t)
	buyerID, trainerAccountID, evaluatorAccountID := uuid.New(), uuid.New(), uuid.New()
	for id, label := range map[uuid.UUID]string{
		buyerID:            "lora-evaluation-buyer",
		trainerAccountID:   "lora-evaluation-trainer-account",
		evaluatorAccountID: "lora-evaluation-evaluator-account",
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`, id, id.String()+"@"+label+".invalid"); err != nil {
			t.Fatal(err)
		}
	}
	trainerSupplierID, evaluatorSupplierID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,owner_buyer_id,status) VALUES
		 ($1,$2,$3,'active'),($4,$5,$6,'active')`,
		trainerSupplierID, trainerSupplierID.String()+"@trainer.invalid", trainerAccountID,
		evaluatorSupplierID, evaluatorSupplierID.String()+"@evaluator.invalid", evaluatorAccountID); err != nil {
		t.Fatal(err)
	}
	trainerWorkerID, evaluatorWorkerID := uuid.New(), uuid.New()
	if _, err := store.CreateWorkerToken(ctx, trainerWorkerID, trainerSupplierID); err != nil {
		t.Fatal(err)
	}
	evaluatorToken, err := store.CreateWorkerToken(ctx, evaluatorWorkerID, evaluatorSupplierID)
	if err != nil {
		t.Fatal(err)
	}
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyerID, "lora-evaluation", true)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(store, nil, nil, nil).Routes()
	archive, heldOutSHA := loraEvaluationProjectTarArchive(t)
	postCompile := func(probe bool, approved string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/compile", bytes.NewReader(archive))
		req.Header.Set("Authorization", "Bearer "+buyerKey)
		req.Header.Set("Content-Type", "application/x-tar")
		if probe {
			req.Header.Set("X-Merc-Bounded-Probe", "true")
			req.Header.Set("X-Merc-Approved-IR-SHA256", approved)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	proposal := postCompile(false, "")
	if proposal.Code != http.StatusOK {
		t.Fatalf("LoRA proposal status=%d body=%s", proposal.Code, proposal.Body.String())
	}
	var proposalIR ProjectWorkloadIR
	if err := json.Unmarshal(proposal.Body.Bytes(), &proposalIR); err != nil {
		t.Fatal(err)
	}
	probe := postCompile(true, proposalIR.IRSHA256)
	if probe.Code != http.StatusOK {
		t.Fatalf("LoRA probe status=%d body=%s", probe.Code, probe.Body.String())
	}
	var probedIR ProjectWorkloadIR
	if err := json.Unmarshal(probe.Body.Bytes(), &probedIR); err != nil {
		t.Fatal(err)
	}
	compileReceiptID, err := uuid.Parse(probe.Header().Get("X-Merc-Project-Compile-Receipt"))
	if err != nil {
		t.Fatalf("LoRA probe omitted compile receipt id: %v", err)
	}
	if got := probedIR.Steps[0].LoRA.HeldOutSet.SHA256; got != heldOutSHA {
		t.Fatalf("probe held-out identity=%s, want %s", got, heldOutSHA)
	}

	submission := ProjectLoRAEvaluationSubmission{
		CompileReceiptID: compileReceiptID, StepID: "train", RunID: uuid.New(),
		TrainerWorkerID: trainerWorkerID, AdapterSHA256: strings.Repeat("a", 64),
		HeldOutSetSHA256: heldOutSHA, BaselineScore: 0.50, CandidateScore: 0.52,
	}
	postEvaluation := func(value ProjectLoRAEvaluationSubmission, token string) *httptest.ResponseRecorder {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/worker/lora/evaluations", bytes.NewReader(raw))
		req.Header.Set("X-Worker-Token", token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	posted := postEvaluation(submission, evaluatorToken)
	if posted.Code != http.StatusCreated {
		t.Fatalf("LoRA evaluation status=%d body=%s", posted.Code, posted.Body.String())
	}
	var receipt ProjectLoRAEvaluationReceipt
	if err := json.Unmarshal(posted.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID == uuid.Nil || receipt.Status != projectLoRAEvaluationReceiptStatus ||
		!receipt.Succeeded || math.Abs(receipt.ImprovementFraction-0.04) > 1e-12 ||
		receipt.EvaluatorWorkerID != evaluatorWorkerID || receipt.TrainerWorkerID != trainerWorkerID ||
		receipt.TrainerAccountID != trainerAccountID || receipt.EvaluatorAccountID != evaluatorAccountID ||
		!strings.Contains(receipt.ExecutionRefusal, "non-executable") || receipt.EvidenceSHA256 == "" {
		t.Fatalf("LoRA evaluation overclaimed or lost identity: %+v", receipt)
	}

	retry := postEvaluation(submission, evaluatorToken)
	var retryReceipt ProjectLoRAEvaluationReceipt
	if retry.Code != http.StatusCreated || json.Unmarshal(retry.Body.Bytes(), &retryReceipt) != nil ||
		retryReceipt.ID != receipt.ID || retryReceipt.EvidenceSHA256 != receipt.EvidenceSHA256 {
		t.Fatalf("LoRA evaluation retry was not idempotent: status=%d body=%s", retry.Code, retry.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/projects/lora/evaluations/"+receipt.ID.String(), nil)
	get.Header.Set("Authorization", "Bearer "+buyerKey)
	got := httptest.NewRecorder()
	handler.ServeHTTP(got, get)
	var replay ProjectLoRAEvaluationReceipt
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &replay) != nil ||
		replay.ID != receipt.ID || replay.EvidenceSHA256 != receipt.EvidenceSHA256 {
		t.Fatalf("buyer LoRA replay failed: status=%d body=%s", got.Code, got.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE project_lora_evaluation_receipts SET succeeded=false WHERE id=$1`, receipt.ID); err == nil {
		t.Fatal("database allowed immutable LoRA evaluation receipt mutation")
	}

	leaked := submission
	leaked.RunID = uuid.New()
	leaked.TrainerSawHeldOutSet = true
	if rec := postEvaluation(leaked, evaluatorToken); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "leaked") {
		t.Fatalf("held-out leakage was not refused: status=%d body=%s", rec.Code, rec.Body.String())
	}
	wrongSet := submission
	wrongSet.RunID = uuid.New()
	wrongSet.HeldOutSetSHA256 = strings.Repeat("b", 64)
	if rec := postEvaluation(wrongSet, evaluatorToken); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "reserved") {
		t.Fatalf("held-out mismatch was not refused: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProjectLoRAEvaluationSupportsLowerIsBetterEvidence(t *testing.T) {
	eval := loraEvaluation{
		RunID: uuid.New(), TrainerSupplierID: uuid.New(), EvaluatorSupplierID: uuid.New(),
		TrainerAccountID: uuid.New(), EvaluatorAccountID: uuid.New(),
		HeldOutSetSHA256: strings.Repeat("a", 64), ReservedSetSHA256: strings.Repeat("a", 64),
		BaselineScore: 0.50, CandidateScore: 0.45, RequiredImprovement: 0.05,
	}
	improvement, succeeded, err := validateProjectLoRAEvaluationEvidence(eval, "LOWER_IS_BETTER")
	if err != nil || !succeeded || math.Abs(improvement-0.1) > 1e-12 {
		t.Fatalf("lower-is-better evidence improvement=%v succeeded=%v err=%v", improvement, succeeded, err)
	}
}
