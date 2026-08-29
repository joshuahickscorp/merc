package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// uiDocumentVersion is the control-owned UI composition schema. Bump only
// when the document shape changes; keep the previous /v1/ui/vN route until
// callers have moved. This surface composes existing handlers. It is not a
// quote, submit, charge, or payout path.
const uiDocumentVersion = 1

// uiComposedFieldSources names the existing handler each composed field is
// copied from. A field with no entry here is either a declared gap or a
// bug. The UI handlers do not compute these values themselves.
var uiComposedFieldSources = map[string]string{
	"buy.identity":           "GET /v1/me",
	"buy.billing":            "GET /v1/billing/status",
	"buy.jobs":               "GET /v1/jobs",
	"earn.earnings":          "GET /v1/worker/earnings",
	"earn.viability":         "GET /v1/worker/viability",
	"health.status":          "GET /healthz",
	"health.runtime":         "GET /version",
	"health.payment":         "GET /readyz",
	"settings.public_config": "GET /v1/public/config",
}

type uiGap struct {
	ID     string `json:"id"`
	Pane   string `json:"pane"`
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"`
}

type uiBuy struct {
	Identity json.RawMessage `json:"identity"`
	Billing  json.RawMessage `json:"billing"`
	Jobs     json.RawMessage `json:"jobs"`
}

type uiEarn struct {
	Earnings  json.RawMessage `json:"earnings"`
	Viability json.RawMessage `json:"viability"`
}

type uiHealth struct {
	Status      json.RawMessage `json:"status"`
	Doctor      json.RawMessage `json:"doctor"`
	Diagnostics json.RawMessage `json:"diagnostics"`
	Network     json.RawMessage `json:"network"`
	Runtime     json.RawMessage `json:"runtime"`
	Payment     json.RawMessage `json:"payment"`
	Warnings    json.RawMessage `json:"warnings"`
}

type uiSettings struct {
	PublicConfig       json.RawMessage `json:"public_config"`
	AppliedPreferences json.RawMessage `json:"applied_preferences"`
}

type uiDocument struct {
	UIVersion int               `json:"ui_version"`
	Surface   string            `json:"surface"`
	Buy       *uiBuy            `json:"buy,omitempty"`
	Earn      *uiEarn           `json:"earn,omitempty"`
	Health    uiHealth          `json:"health"`
	Settings  uiSettings        `json:"settings"`
	Sources   map[string]string `json:"sources"`
	Gaps      []uiGap           `json:"gaps"`
}

type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// captureHandlerJSON invokes an existing handler on a clone of r and returns
// its status and body. The UI facade uses this so pane fields are the handler
// output, not a second computation of the same facts.
func captureHandlerJSON(r *http.Request, h http.HandlerFunc) (int, json.RawMessage) {
	w := &captureWriter{header: make(http.Header)}
	h(w, r.Clone(r.Context()))
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, json.RawMessage(bytes.TrimSpace(w.body.Bytes()))
}

func writeCaptured(w http.ResponseWriter, status int, body json.RawMessage) {
	if len(body) == 0 {
		writeErr(w, status, "ui source failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func copyReadyzPaymentFields(readyz json.RawMessage) json.RawMessage {
	var obj map[string]any
	if json.Unmarshal(readyz, &obj) != nil {
		return nil
	}
	out := make(map[string]any, len(readyzPaymentFieldNames))
	for _, key := range readyzPaymentFieldNames {
		if value, ok := obj[key]; ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

func uiFixedGaps() []uiGap {
	return []uiGap{
		{
			ID: "health.doctor", Pane: "health", Field: "doctor",
			Reason: "no HTTP doctor handler exists; ops/scripts/release-doctor.sh is a CLI-only operator check",
		},
		{
			ID: "health.diagnostics", Pane: "health", Field: "diagnostics",
			Reason: "no tenant-scoped diagnostics handler exists",
		},
		{
			ID: "health.network", Pane: "health", Field: "network",
			Reason: "GET /admin/market/liquidity/network is operator-only; composing it here would grant new authority",
			Source: "GET /admin/market/liquidity/network",
		},
		{
			ID: "health.warnings", Pane: "health", Field: "warnings",
			Reason: "no standalone warnings handler; quote warnings require POST /v1/quote, which this surface must not call",
			Source: "POST /v1/quote",
		},
		{
			ID: "settings.applied_preferences", Pane: "settings", Field: "applied_preferences",
			Reason: "no user preference store or handler exists",
		},
	}
}

func uiCrossIdentityGaps(surface string) []uiGap {
	if surface == "buy" {
		return []uiGap{
			{
				ID: "earn.earnings", Pane: "earn", Field: "earnings",
				Reason: "earnings are worker-owned; present X-Worker-Token to GET /v1/ui/v1/earn",
				Source: "GET /v1/worker/earnings",
			},
			{
				ID: "earn.viability", Pane: "earn", Field: "viability",
				Reason: "viability is worker-owned; present X-Worker-Token to GET /v1/ui/v1/earn",
				Source: "GET /v1/worker/viability",
			},
		}
	}
	return []uiGap{
		{
			ID: "buy.identity", Pane: "buy", Field: "identity",
			Reason: "identity is buyer-owned; present a buyer bearer token to GET /v1/ui/v1/buy",
			Source: "GET /v1/me",
		},
		{
			ID: "buy.billing", Pane: "buy", Field: "billing",
			Reason: "billing status is buyer-owned; present a buyer bearer token to GET /v1/ui/v1/buy",
			Source: "GET /v1/billing/status",
		},
		{
			ID: "buy.jobs", Pane: "buy", Field: "jobs",
			Reason: "jobs are buyer-owned; present a buyer bearer token to GET /v1/ui/v1/buy",
			Source: "GET /v1/jobs",
		},
	}
}

func uiSourcesForSurface(surface string) map[string]string {
	out := make(map[string]string, len(uiComposedFieldSources))
	for path, source := range uiComposedFieldSources {
		if surface == "buy" && strings.HasPrefix(path, "earn.") {
			continue
		}
		if surface == "earn" && strings.HasPrefix(path, "buy.") {
			continue
		}
		out[path] = source
	}
	return out
}

func (s *Server) composeUIHealth(r *http.Request) (uiHealth, []uiGap) {
	_, statusBody := captureHandlerJSON(r, s.handleHealthz)
	_, runtimeBody := captureHandlerJSON(r, s.handleVersion)
	_, readyzBody := captureHandlerJSON(r, s.handleReadyz)
	payment := copyReadyzPaymentFields(readyzBody)
	var gaps []uiGap
	if payment == nil {
		gaps = append(gaps, uiGap{
			ID: "health.payment", Pane: "health", Field: "payment",
			Reason: "GET /readyz omitted payment fields because payment authority did not parse or returned early",
			Source: "GET /readyz",
		})
	}
	return uiHealth{
		Status:  statusBody,
		Runtime: runtimeBody,
		Payment: payment,
	}, gaps
}

func (s *Server) composeUISettings(r *http.Request) uiSettings {
	_, config := captureHandlerJSON(r, s.handlePublicConfig)
	return uiSettings{PublicConfig: config}
}

func (s *Server) handleUIBuy(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(ctxBuyer) == nil {
		writeErr(w, http.StatusUnauthorized, "invalid credential")
		return
	}
	status, identity := captureHandlerJSON(r, s.handleMe)
	if status != http.StatusOK {
		writeCaptured(w, status, identity)
		return
	}
	status, billing := captureHandlerJSON(r, s.handleBillingStatus)
	if status != http.StatusOK {
		writeCaptured(w, status, billing)
		return
	}
	status, jobs := captureHandlerJSON(r, s.handleListBuyerJobs)
	if status != http.StatusOK {
		writeCaptured(w, status, jobs)
		return
	}
	health, healthGaps := s.composeUIHealth(r)
	gaps := append(uiFixedGaps(), uiCrossIdentityGaps("buy")...)
	gaps = append(gaps, healthGaps...)
	writeJSON(w, http.StatusOK, uiDocument{
		UIVersion: uiDocumentVersion,
		Surface:   "buy",
		Buy:       &uiBuy{Identity: identity, Billing: billing, Jobs: jobs},
		Health:    health,
		Settings:  s.composeUISettings(r),
		Sources:   uiSourcesForSurface("buy"),
		Gaps:      gaps,
	})
}

func (s *Server) handleUIEarn(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(ctxWorker) == nil {
		writeErr(w, http.StatusUnauthorized, "invalid credential")
		return
	}
	status, earnings := captureHandlerJSON(r, s.handleWorkerEarnings)
	if status != http.StatusOK {
		writeCaptured(w, status, earnings)
		return
	}
	status, viability := captureHandlerJSON(r, s.handleWorkerViability)
	if status != http.StatusOK {
		writeCaptured(w, status, viability)
		return
	}
	health, healthGaps := s.composeUIHealth(r)
	gaps := append(uiFixedGaps(), uiCrossIdentityGaps("earn")...)
	gaps = append(gaps, healthGaps...)
	writeJSON(w, http.StatusOK, uiDocument{
		UIVersion: uiDocumentVersion,
		Surface:   "earn",
		Earn:      &uiEarn{Earnings: earnings, Viability: viability},
		Health:    health,
		Settings:  s.composeUISettings(r),
		Sources:   uiSourcesForSurface("earn"),
		Gaps:      gaps,
	})
}
