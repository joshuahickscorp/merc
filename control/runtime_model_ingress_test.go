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
	// Only candle's hf cell is advertised for MiniLM today. Naming gguf is still
	// a 400 until a gguf cell is promoted into the advertised set — the wire-kind
	// fix accepts any advertised kind, not any kind that exists in the document.
	_, herr := (&Server{}).createJob(context.Background(), uuid.New(), jobSubmit{
		JobType: JobType{Type: "embed"},
		Model:   ModelRef{Kind: "gguf", Ref: "all-minilm-l6-v2"},
	})
	if herr == nil || herr.status != http.StatusBadRequest ||
		!strings.Contains(herr.msg, `no advertised cell serving model.kind="gguf"`) {
		t.Fatalf("createJob mismatch result=%v, want unadvertised-kind 400", herr)
	}
}

func TestQuoteRejectsExplicitNoncanonicalModelKindBeforeSideEffects(t *testing.T) {
	body := []byte(`{"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"all-minilm-l6-v2"},"input":"{\"text\":\"x\"}\n"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/quote", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxBuyer, &AuthResult{BuyerID: uuid.New()}))
	rec := httptest.NewRecorder()

	(&Server{}).handleQuote(rec, req)

	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), `no advertised cell serving model.kind=\"gguf\"`) {
		t.Fatalf("quote mismatch status=%d body=%s, want unadvertised-kind 400", rec.Code, rec.Body.String())
	}
}
