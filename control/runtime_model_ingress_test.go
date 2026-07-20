package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCreateJobRejectsExplicitNoncanonicalModelKindBeforeSideEffects(t *testing.T) {
	_, herr := (&Server{}).createJob(context.Background(), uuid.New(), jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "gguf", Ref: "all-minilm-l6-v2"},
	})
	if herr == nil || herr.status != http.StatusBadRequest ||
		!strings.Contains(herr.msg, `requires model.kind="hf"`) {
		t.Fatalf("createJob mismatch result=%v, want early canonical-kind 400", herr)
	}
}

func TestQuoteRejectsExplicitNoncanonicalModelKindBeforeSideEffects(t *testing.T) {
	body := []byte(`{"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"all-minilm-l6-v2"},"input":"{\"text\":\"x\"}\n"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: uuid.New()}))
	rec := httptest.NewRecorder()

	(&Server{}).handleQuote(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `requires model.kind=\"hf\"`) {
		t.Fatalf("quote mismatch status=%d body=%s, want early canonical-kind 400", rec.Code, rec.Body.String())
	}
}
