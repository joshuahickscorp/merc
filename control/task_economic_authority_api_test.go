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
	ctx, store, _ := currentPhysicalCatalogueFixture(t)
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
	must(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM quotes`).Scan(&rows))
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
	server.canary = CanaryPolicy{
		Enabled: true, MaxOutputTokens: 32,
		MaxJobDurationSecs: defaultMaxJobDurationSecs,
		MaxShadowValueUSD:  5,
	}
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
			!strings.Contains(recorder.Body.String(), "private canary requires a heterogeneous honeypot") {
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
