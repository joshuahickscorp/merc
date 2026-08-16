package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestReadyzPaymentFieldNamesMatchBuilder(t *testing.T) {
	fields := readyzPaymentFields(PaymentAuthority{Mode: PaymentModeSealed})
	if len(fields) != len(readyzPaymentFieldNames) {
		t.Fatalf("readyz payment fields = %d keys, named %d", len(fields), len(readyzPaymentFieldNames))
	}
	for _, key := range readyzPaymentFieldNames {
		if _, ok := fields[key]; !ok {
			t.Fatalf("readyzPaymentFields missing named key %q", key)
		}
	}
}

func TestUIFacadeRoutesAreVersionedAndCredentialed(t *testing.T) {
	handler := NewServer(nil, nil, nil, nil).Routes()
	for _, path := range []string{"/v1/ui/v1/buy", "/v1/ui/v1/earn"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: got %d, want 401", path, rec.Code)
		}
	}
	for _, path := range []string{"/v1/ui/buy", "/v1/ui/v2/buy", "/v1/ui/v2/earn", "/v1/ui/v1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer cx_not_a_key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404 (only /v1/ui/v1/{buy,earn} exist)", path, rec.Code)
		}
	}
	post := httptest.NewRequest(http.MethodPost, "/v1/ui/v1/buy", bytes.NewReader([]byte(`{}`)))
	post.Header.Set("Authorization", "Bearer cx_not_a_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, post)
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/ui/v1/buy: got %d, want 405 or 404 (read surface)", rec.Code)
	}
}

func TestUIFacadeSourceHasNoWriteOrQuoteAuthority(t *testing.T) {
	src, err := os.ReadFile("ui_facade.go")
	must(t, err)
	for _, banned := range []string{
		"handleQuote",
		"handleCreateJob",
		"handleBillingTopup",
		"handleBillingSetup",
		"handleChatCompletions",
		"handleImageGenerations",
		"chargePaymentIntent",
		"SubmitJobTx",
		"BuildEconomicPlan",
		"handleAdmin",
		"handleCreateProjectOrder",
		"handleCreateServiceLease",
		"handleCreateExecutionEnvelope",
	} {
		if bytes.Contains(src, []byte(banned)) {
			t.Errorf("ui facade must not call %s (new authority)", banned)
		}
	}
}

func TestUIFacadeFixedGapsNameMissingSources(t *testing.T) {
	got := map[string]uiGap{}
	for _, gap := range uiFixedGaps() {
		got[gap.ID] = gap
	}
	for _, id := range []string{
		"health.doctor",
		"health.diagnostics",
		"health.network",
		"health.warnings",
		"settings.applied_preferences",
	} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing required gap %q", id)
		}
	}
	if gap := got["health.network"]; !strings.Contains(gap.Reason, "operator-only") {
		t.Fatalf("network gap must refuse operator composition, got %q", gap.Reason)
	}
	if gap := got["health.warnings"]; !strings.Contains(gap.Reason, "POST /v1/quote") {
		t.Fatalf("warnings gap must refuse quote recomputation, got %q", gap.Reason)
	}
}

func TestUIFacadeComposesExistingSourcesWithoutRecompute(t *testing.T) {
	installSettlementCurrencyForTest(t, "cad")
	ctx, store, pool := openIsolatedTestStore(t)
	buyer, err := store.CreateBuyerAccount(ctx, "ui-facade-"+uuid.NewString()+"@example.test", "integration-password", 0)
	must(t, err)
	_, buyerKey, _, err := store.CreateAPIKey(ctx, buyer, "ui-facade", true)
	must(t, err)
	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs
		    (id,buyer_id,status,job_type,model_ref,input_ref,tier,estimated_usd,actual_usd,task_count,tasks_done,currency,charge_status,budget_state)
		VALUES ($1,$2,'queued','embed','all-minilm-l6-v2','ui/input','batch',0.01,0,$3,0,$4,'none','open')`,
		jobID, buyer, 1, SettlementCurrencyCode()); err != nil {
		t.Fatalf("insert buyer job: %v", err)
	}
	ownerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO buyers (id,email) VALUES ($1,$2)`,
		ownerID, ownerID.String()+"@ui-facade-owner.invalid"); err != nil {
		t.Fatalf("insert supplier owner: %v", err)
	}
	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,owner_buyer_id,status) VALUES ($1,$2,$3,'active')`,
		supplierID, supplierID.String()+"@ui-facade-supplier.invalid", ownerID); err != nil {
		t.Fatalf("insert supplier: %v", err)
	}
	workerID := uuid.New()
	workerToken, err := store.CreateWorkerToken(ctx, workerID, supplierID)
	must(t, err)

	handler := NewServer(store, nil, nil, nil).Routes()
	get := func(method, path, bearer, worker string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if worker != "" {
			req.Header.Set("X-Worker-Token", worker)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(http.MethodGet, "/v1/ui/v1/buy", "", workerToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("worker token on buy: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(http.MethodGet, "/v1/ui/v1/earn", buyerKey, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("buyer key on earn: %d %s", rec.Code, rec.Body.String())
	}

	buyRec := get(http.MethodGet, "/v1/ui/v1/buy", buyerKey, "")
	if buyRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/ui/v1/buy: %d %s", buyRec.Code, buyRec.Body.String())
	}
	earnRec := get(http.MethodGet, "/v1/ui/v1/earn", "", workerToken)
	if earnRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/ui/v1/earn: %d %s", earnRec.Code, earnRec.Body.String())
	}

	var buyDoc, earnDoc map[string]any
	must(t, json.Unmarshal(buyRec.Body.Bytes(), &buyDoc))
	must(t, json.Unmarshal(earnRec.Body.Bytes(), &earnDoc))
	if buyDoc["ui_version"] != float64(uiDocumentVersion) || buyDoc["surface"] != "buy" {
		t.Fatalf("buy document identity: %#v", buyDoc)
	}
	if earnDoc["ui_version"] != float64(uiDocumentVersion) || earnDoc["surface"] != "earn" {
		t.Fatalf("earn document identity: %#v", earnDoc)
	}
	assertUIGapsInclude(t, buyDoc, "health.doctor", "health.diagnostics", "health.network",
		"health.warnings", "settings.applied_preferences", "earn.earnings", "earn.viability")
	assertUIGapsInclude(t, earnDoc, "health.doctor", "settings.applied_preferences",
		"buy.identity", "buy.billing", "buy.jobs")

	compare := func(doc map[string]any, path, sourcePath, bearer, worker string, pickPayment bool) {
		t.Helper()
		source := get(http.MethodGet, sourcePath, bearer, worker)
		if source.Code != http.StatusOK && !(pickPayment && source.Code == http.StatusServiceUnavailable) {
			t.Fatalf("%s: source %s status %d %s", path, sourcePath, source.Code, source.Body.String())
		}
		var want any
		must(t, json.Unmarshal(source.Body.Bytes(), &want))
		if pickPayment {
			obj, ok := want.(map[string]any)
			if !ok {
				t.Fatalf("%s: /readyz body is not an object: %#v", path, want)
			}
			payment := map[string]any{}
			for _, key := range readyzPaymentFieldNames {
				if value, ok := obj[key]; ok {
					payment[key] = value
				}
			}
			if len(payment) == 0 {
				if jsonPath(doc, path) != nil {
					t.Fatalf("%s: want null payment when /readyz omits payment fields, got %#v", path, jsonPath(doc, path))
				}
				return
			}
			want = payment
		}
		got := jsonPath(doc, path)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s was not copied from %s\ngot:  %#v\nwant: %#v", path, sourcePath, got, want)
		}
	}

	compare(buyDoc, "buy.identity", "/v1/me", buyerKey, "", false)
	compare(buyDoc, "buy.billing", "/v1/billing/status", buyerKey, "", false)
	compare(buyDoc, "buy.jobs", "/v1/jobs", buyerKey, "", false)
	compare(buyDoc, "health.status", "/healthz", buyerKey, "", false)
	compare(buyDoc, "health.runtime", "/version", buyerKey, "", false)
	compare(buyDoc, "health.payment", "/readyz", buyerKey, "", true)
	compare(buyDoc, "settings.public_config", "/v1/public/config", buyerKey, "", false)

	compare(earnDoc, "earn.earnings", "/v1/worker/earnings", "", workerToken, false)
	compare(earnDoc, "earn.viability", "/v1/worker/viability", "", workerToken, false)
	compare(earnDoc, "health.status", "/healthz", "", workerToken, false)
	compare(earnDoc, "health.runtime", "/version", "", workerToken, false)
	compare(earnDoc, "health.payment", "/readyz", "", workerToken, true)
	compare(earnDoc, "settings.public_config", "/v1/public/config", "", workerToken, false)

	if jsonPath(buyDoc, "buy.identity.buyer_id") != buyer.String() {
		t.Fatalf("buy identity is not the authenticated buyer: %#v", jsonPath(buyDoc, "buy.identity"))
	}
	if jsonPath(buyDoc, "health.doctor") != nil || jsonPath(buyDoc, "settings.applied_preferences") != nil {
		t.Fatalf("gap fields were invented: doctor=%#v prefs=%#v",
			jsonPath(buyDoc, "health.doctor"), jsonPath(buyDoc, "settings.applied_preferences"))
	}
	if jsonPath(buyDoc, "earn") != nil {
		t.Fatalf("buy document leaked earn authority: %#v", jsonPath(buyDoc, "earn"))
	}
	if jsonPath(earnDoc, "buy") != nil {
		t.Fatalf("earn document leaked buy authority: %#v", jsonPath(earnDoc, "buy"))
	}

	sources, _ := buyDoc["sources"].(map[string]any)
	if sources["buy.identity"] != "GET /v1/me" || sources["buy.billing"] != "GET /v1/billing/status" {
		t.Fatalf("buy sources lost provenance: %#v", sources)
	}
	if _, ok := sources["earn.earnings"]; ok {
		t.Fatalf("buy sources listed worker-owned earn fields: %#v", sources)
	}
}

func assertUIGapsInclude(t *testing.T, doc map[string]any, ids ...string) {
	t.Helper()
	raw, ok := doc["gaps"].([]any)
	if !ok {
		t.Fatalf("document has no gaps array: %#v", doc["gaps"])
	}
	have := map[string]bool{}
	for _, item := range raw {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		have[id] = true
	}
	for _, id := range ids {
		if !have[id] {
			t.Errorf("document missing gap %q", id)
		}
	}
}

func jsonPath(v any, path string) any {
	cur := v
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}
