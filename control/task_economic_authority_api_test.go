package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestQuoteRefusesUnevenTaskEconomicsWith503AndZeroWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	// Isolated DB so zero-writes is meaningful. TEST_ONLY publication + combined
	// token keep the request advertised so the uniform task-economics gate is the
	// refusal under test.
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build uneven-quote catalogue: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish uneven-quote catalogue: %v", err)
	}
	body := testOnlyBatchPublicRequest(strangerBatchCorpus, 1)
	body["params"] = map[string]any{"split_size": 1} // three primaries, each paid differently in reality
	raw, err := json.Marshal(body)
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(
		req.Context(), ctxBuyer, &AuthResult{BuyerID: uuid.New()},
	))
	recorder := httptest.NewRecorder()
	NewServer(store, nil, nil, nil).handleQuote(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "exact heterogeneous per-task economics") {
		t.Fatalf("uneven quote status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM quotes`).Scan(&rows))
	if rows != 0 {
		t.Fatalf("uneven quote wrote %d quote rows", rows)
	}
}

func TestUnquotedSubmitRefusesUnevenTaskEconomicsWith503AndZeroWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build physical catalogue schedule: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish physical catalogue schedule: %v")
	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "uneven-task-submit-"+buyerID.String()+"@test")
	mustf(t, err, "insert uneven-task buyer: %v")

	body := testOnlyBatchPublicRequest(strangerBatchCorpus, 1)
	body["params"] = map[string]any{"split_size": 1}
	raw, err := json.Marshal(body)
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(raw))
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req = req.WithContext(context.WithValue(
		req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
	))
	recorder := httptest.NewRecorder()
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	NewServer(store, artifacts.storage, verifier, nil).handleCreateJob(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "exact heterogeneous per-task economics") {
		t.Fatalf("uneven submit status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for table, query := range map[string]string{
		"jobs":     `SELECT count(*) FROM jobs`,
		"tasks":    `SELECT count(*) FROM tasks`,
		"reserves": `SELECT count(*) FROM job_economic_reserves`,
	} {
		var rows int
		must(t, pool.QueryRow(ctx, query).Scan(&rows))
		if rows != 0 {
			t.Fatalf("uneven submit left %d %s rows", rows, table)
		}
	}
}

func TestSubmitRefusesHoneypotEconomicsBeforeRegisteringOpaqueAlias(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build physical catalogue schedule: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish physical catalogue schedule: %v")
	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "honeypot-uniform-refusal-"+buyerID.String()+"@test")
	mustf(t, err, "insert honeypot refusal buyer: %v")

	seedKey := "honeypots/uniform-task-refusal.jsonl"
	seedInput := []byte("{\"prompt\":\"known answer probe\"}\n")
	mustf(t, artifacts.storage.PutObject(ctx, seedKey, seedInput,
		"application/x-ndjson"), "store honeypot seed: %v")
	_, err = pool.Exec(ctx, `
		INSERT INTO honeypots (job_type,input_ref,known_answer,answer_class,answer_model)
		VALUES ('batch_infer',$1,'known'::bytea,'candle|test','llama-3.2-1b-instruct-q4')`, seedKey)
	mustf(t, err, "seed honeypot: %v")

	body := testOnlyBatchPublicRequest(
		"{\"id\":\"0\",\"prompt\":\"one exact primary\"}\n", 1)
	body["constraints"] = map[string]any{"max_duration_secs": 3600}
	body["verification"] = map[string]any{
		"redundancy_frac": 1.0,
		"honeypot_frac":   1.0,
	}
	raw, err := json.Marshal(body)
	must(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(raw))
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req = req.WithContext(context.WithValue(
		req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
	))
	recorder := httptest.NewRecorder()
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	NewServer(store, artifacts.storage, verifier, nil).handleCreateJob(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "heterogeneous honeypot") {
		t.Fatalf("honeypot economics status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var aliases, jobs, tasks, reserves int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM honeypots WHERE input_ref LIKE 'jobs/%'`).Scan(&aliases))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobs))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM job_economic_reserves`).Scan(&reserves))
	if aliases != 0 || jobs != 0 || tasks != 0 || reserves != 0 {
		t.Fatalf("honeypot refusal left aliases=%d jobs=%d tasks=%d reserves=%d",
			aliases, jobs, tasks, reserves)
	}
}

func TestCanaryUniformEconomicsRefusesQuoteAndSubmitBeforeWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build canary refusal catalogue: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish canary refusal catalogue: %v")
	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "canary-uniform-refusal-"+buyerID.String()+"@test")
	mustf(t, err, "insert canary refusal buyer: %v")

	body := testOnlyBatchPublicRequest(
		"{\"id\":\"0\",\"prompt\":\"one exact primary\"}\n", 1)
	body["constraints"] = map[string]any{"max_duration_secs": 3600}
	body["verification"] = map[string]any{"redundancy_frac": 1.0}
	raw, err := json.Marshal(body)
	must(t, err)

	server := NewServer(store, nil, nil, nil)
	// Narrower refusal: this envelope names a worker that is not
	// operator-reserved, so it admits an independent supplier. The honeypot
	// requirement stays in force and uniform v1 still cannot allocate it.
	server.canary = independentSupplierCanaryForTest()
	for _, endpoint := range []string{"/v1/quote", "/v1/jobs"} {
		req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req = req.WithContext(context.WithValue(
			req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
		))
		recorder := httptest.NewRecorder()
		if endpoint == "/v1/quote" {
			server.handleQuote(recorder, req)
		} else {
			server.handleCreateJob(recorder, req)
		}
		if recorder.Code != http.StatusServiceUnavailable ||
			!strings.Contains(recorder.Body.String(), "private canary requires a heterogeneous honeypot") ||
			!strings.Contains(recorder.Body.String(), "not operator-controlled") {
			t.Fatalf("%s status=%d body=%s", endpoint, recorder.Code, recorder.Body.String())
		}
	}

	for table, query := range map[string]string{
		"quotes":           `SELECT count(*) FROM quotes`,
		"jobs":             `SELECT count(*) FROM jobs`,
		"tasks":            `SELECT count(*) FROM tasks`,
		"reserves":         `SELECT count(*) FROM job_economic_reserves`,
		"honeypot aliases": `SELECT count(*) FROM honeypots WHERE input_ref LIKE 'jobs/%'`,
	} {
		var rows int
		must(t, pool.QueryRow(ctx, query).Scan(&rows))
		if rows != 0 {
			t.Fatalf("canary refusal left %d %s", rows, table)
		}
	}
}

func TestCanaryOperatorControlledSingleSupplierRefusesQuoteAndSubmitBeforeWrites(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build single-supplier canary catalogue: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish single-supplier canary catalogue: %v")
	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "canary-single-supplier-"+buyerID.String()+"@test")
	mustf(t, err, "insert single-supplier canary buyer: %v")

	body := testOnlyBatchPublicRequest(
		"{\"id\":\"0\",\"prompt\":\"one exact primary\"}\n", 1)
	body["constraints"] = map[string]any{"max_duration_secs": 3600}
	body["verification"] = map[string]any{"redundancy_frac": 1.0}
	raw, err := json.Marshal(body)
	must(t, err)

	server := NewServer(store, nil, nil, nil)
	// Staging-shaped envelope: operator-reserved, but only one supplier can
	// be admitted. Redundancy cannot be independent, so the honeypot
	// requirement stands and quote/submit refuse here — not later as
	// NO_INDEPENDENT_SUPPLIER after work has run.
	server.canary = singleOperatorControlledCanaryForTest()
	for _, endpoint := range []string{"/v1/quote", "/v1/jobs"} {
		req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req = req.WithContext(context.WithValue(
			req.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
		))
		recorder := httptest.NewRecorder()
		if endpoint == "/v1/quote" {
			server.handleQuote(recorder, req)
		} else {
			server.handleCreateJob(recorder, req)
		}
		if recorder.Code != http.StatusServiceUnavailable ||
			!strings.Contains(recorder.Body.String(), "insufficient independent suppliers") ||
			!strings.Contains(recorder.Body.String(), "NO_INDEPENDENT_SUPPLIER") {
			t.Fatalf("%s status=%d body=%s", endpoint, recorder.Code, recorder.Body.String())
		}
	}

	for table, query := range map[string]string{
		"quotes":           `SELECT count(*) FROM quotes`,
		"jobs":             `SELECT count(*) FROM jobs`,
		"tasks":            `SELECT count(*) FROM tasks`,
		"reserves":         `SELECT count(*) FROM job_economic_reserves`,
		"honeypot aliases": `SELECT count(*) FROM honeypots WHERE input_ref LIKE 'jobs/%'`,
	} {
		var rows int
		must(t, pool.QueryRow(ctx, query).Scan(&rows))
		if rows != 0 {
			t.Fatalf("single-supplier canary refusal left %d %s", rows, table)
		}
	}
}

func TestCanaryOperatorControlledAllowsQuoteAndSubmitWithRedundancyOnly(t *testing.T) {
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	installTestOnlyCombinedTokenAuthority(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build operator-controlled canary catalogue: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "publish operator-controlled canary catalogue: %v")
	buyerID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO buyers (id,email,password_hash,free_credit_usd) VALUES ($1,$2,'x',5)`,
		buyerID, "canary-operator-controlled-"+buyerID.String()+"@test")
	mustf(t, err, "insert operator-controlled canary buyer: %v")

	body := testOnlyBatchPublicRequest(
		"{\"id\":\"0\",\"prompt\":\"one exact primary\"}\n", 1)
	body["constraints"] = map[string]any{"max_duration_secs": 3600}
	body["verification"] = map[string]any{
		"redundancy_frac": 1.0,
		"honeypot_frac":   0.0,
	}
	raw, err := json.Marshal(body)
	must(t, err)

	quoteServer := NewServer(store, nil, nil, nil)
	quoteServer.canary = operatorControlledCanaryForTest()
	quoteReq := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(raw))
	quoteReq = quoteReq.WithContext(context.WithValue(
		quoteReq.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
	))
	quoteRec := httptest.NewRecorder()
	quoteServer.handleQuote(quoteRec, quoteReq)
	if quoteRec.Code != http.StatusOK {
		t.Fatalf("operator-controlled canary quote status=%d body=%s",
			quoteRec.Code, quoteRec.Body.String())
	}
	var quoted Quote
	mustf(t, json.Unmarshal(quoteRec.Body.Bytes(), &quoted), "decode quote: %v")
	if quoted.QuoteID == "" {
		t.Fatalf("quote missing quote_id: %s", quoteRec.Body.String())
	}
	if quoted.ComputePlan.HoneypotTasks != 0 || quoted.ComputePlan.RedundancyTasks < 1 {
		t.Fatalf("quote compute geometry honeypot=%d redundancy=%d, want 0 honeypot and at least one redundancy clone",
			quoted.ComputePlan.HoneypotTasks, quoted.ComputePlan.RedundancyTasks)
	}

	artifacts := newArtifactHarness(t)
	verifier := NewVerifier(store).WithStorage(artifacts.storage)
	server := NewServer(store, artifacts.storage, verifier, nil)
	server.canary = operatorControlledCanaryForTest()
	submitReq := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(raw))
	submitReq.Header.Set("Idempotency-Key", uuid.NewString())
	submitReq = submitReq.WithContext(context.WithValue(
		submitReq.Context(), ctxBuyer, &AuthResult{BuyerID: buyerID},
	))
	submitRec := httptest.NewRecorder()
	server.handleCreateJob(submitRec, submitReq)
	if submitRec.Code != http.StatusOK && submitRec.Code != http.StatusCreated &&
		submitRec.Code != http.StatusAccepted {
		t.Fatalf("operator-controlled canary submit status=%d body=%s",
			submitRec.Code, submitRec.Body.String())
	}
	var jobs, honeypots int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobs))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE is_honeypot`).Scan(&honeypots))
	if jobs != 1 {
		t.Fatalf("operator-controlled submit wrote %d jobs, want 1", jobs)
	}
	if honeypots != 0 {
		t.Fatalf("operator-controlled submit admitted %d honeypot tasks", honeypots)
	}
}
