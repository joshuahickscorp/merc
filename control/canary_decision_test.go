package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCandidateCommit = "0123456789abcdef0123456789abcdef01234567"

func testCleanBuild() ControlBuildInfo {
	return ControlBuildInfo{Commit: testCandidateCommit}
}

func validTestDecision() canaryDisableDecisionEnvelope {
	return canaryDisableDecisionEnvelope{
		SchemaVersion: 1,
		Decision: canaryDisableDecision{
			DecisionID:      "INC-canary-exit-1",
			CandidateCommit: testCandidateCommit,
			Decision:        canaryDisableDecisionVerb,
			Scope:           canaryDisableDecisionScope,
			Authority: []canaryDecisionAuthority{
				{Role: "release_manager", Approver: "release manager", Reference: "https://example.test/rel/1"},
				{Role: "security", Approver: "security owner", Reference: "https://example.test/sec/1"},
			},
			EffectiveAt: "2026-01-01T00:00:00Z",
			ExpiresAt:   "2026-02-01T00:00:00Z",
		},
	}
}

// writeTestDecision writes the artifact and declares its digest, which is the
// configuration an operator would produce.
func writeTestDecision(t *testing.T, envelope canaryDisableDecisionEnvelope) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode decision: %v", err)
	}
	path := filepath.Join(t.TempDir(), "canary-disable-decision.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write decision: %v", err)
	}
	sum := sha256.Sum256(raw)
	t.Setenv(canaryDisableDecisionDigestEnv, hex.EncodeToString(sum[:]))
	return path
}

// A bare reference is what production used to accept. Every case below has to
// come back with its own refusal, because "the canary will not turn off" is an
// operator's whole diagnosis otherwise.
func TestCanaryDisableDecisionRefusalsAreDistinct(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	t.Run("valid", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); err != nil {
			t.Fatalf("governed decision was refused: %v", err)
		}
	})

	t.Run("open_ended_valid", func(t *testing.T) {
		envelope := validTestDecision()
		envelope.Decision.ExpiresAt = ""
		path := writeTestDecision(t, envelope)
		if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); err != nil {
			t.Fatalf("decision without an expiry was refused: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if err := resolveCanaryDisableDecision("", true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionMissing) {
			t.Fatalf("absent reference: %v", err)
		}
	})

	t.Run("bare_string", func(t *testing.T) {
		writeTestDecision(t, validTestDecision())
		if err := resolveCanaryDisableDecision("yes", true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionUnreadable) {
			t.Fatalf("bare string accepted in production: %v", err)
		}
	})

	t.Run("undeclared_digest", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		t.Setenv(canaryDisableDecisionDigestEnv, "")
		if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionUnverified) {
			t.Fatalf("decision with no declared digest: %v", err)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		tampered := validTestDecision()
		tampered.Decision.DecisionID = "INC-canary-exit-forged"
		raw, err := json.Marshal(tampered)
		if err != nil {
			t.Fatalf("encode tampered decision: %v", err)
		}
		// The declared digest still describes the bytes that were approved.
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("rewrite decision: %v", err)
		}
		if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionTampered) {
			t.Fatalf("rewritten decision file: %v", err)
		}
	})

	t.Run("other_candidate", func(t *testing.T) {
		envelope := validTestDecision()
		envelope.Decision.CandidateCommit = "89abcdef0123456789abcdef0123456789abcdef"
		path := writeTestDecision(t, envelope)
		if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionWrongCandidate) {
			t.Fatalf("decision about another candidate: %v", err)
		}
	})

	t.Run("modified_build", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		dirty := testCleanBuild()
		dirty.Modified = true
		if err := resolveCanaryDisableDecision(path, true, now, func() ControlBuildInfo { return dirty }); !errors.Is(err, errCanaryDecisionWrongCandidate) {
			t.Fatalf("decision applied to a modified build: %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		late := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		if err := resolveCanaryDisableDecision(path, true, late, testCleanBuild); !errors.Is(err, errCanaryDecisionExpired) {
			t.Fatalf("expired decision: %v", err)
		}
	})

	t.Run("not_in_force", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		early := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
		if err := resolveCanaryDisableDecision(path, true, early, testCleanBuild); !errors.Is(err, errCanaryDecisionNotInForce) {
			t.Fatalf("decision that is not in force yet: %v", err)
		}
	})

	for name, mutate := range map[string]func(*canaryDisableDecisionEnvelope){
		"wrong_schema":     func(e *canaryDisableDecisionEnvelope) { e.SchemaVersion = 2 },
		"wrong_verb":       func(e *canaryDisableDecisionEnvelope) { e.Decision.Decision = "SOMETHING_ELSE" },
		"wrong_scope":      func(e *canaryDisableDecisionEnvelope) { e.Decision.Scope = "some_other_deployment" },
		"no_receipt_id":    func(e *canaryDisableDecisionEnvelope) { e.Decision.DecisionID = "" },
		"short_commit":     func(e *canaryDisableDecisionEnvelope) { e.Decision.CandidateCommit = "0123456" },
		"one_approver":     func(e *canaryDisableDecisionEnvelope) { e.Decision.Authority = e.Decision.Authority[:1] },
		"duplicate_role":   func(e *canaryDisableDecisionEnvelope) { e.Decision.Authority[1].Role = "release_manager" },
		"unknown_role":     func(e *canaryDisableDecisionEnvelope) { e.Decision.Authority[1].Role = "a_friend" },
		"blank_reference":  func(e *canaryDisableDecisionEnvelope) { e.Decision.Authority[0].Reference = "" },
		"local_timestamp":  func(e *canaryDisableDecisionEnvelope) { e.Decision.EffectiveAt = "2026-01-01T00:00:00+05:00" },
		"expiry_precedes":  func(e *canaryDisableDecisionEnvelope) { e.Decision.ExpiresAt = "2025-06-01T00:00:00Z" },
		"no_effective_at":  func(e *canaryDisableDecisionEnvelope) { e.Decision.EffectiveAt = "" },
		"control_char_ref": func(e *canaryDisableDecisionEnvelope) { e.Decision.Authority[0].Approver = "release\nmanager" },
	} {
		t.Run(name, func(t *testing.T) {
			envelope := validTestDecision()
			mutate(&envelope)
			path := writeTestDecision(t, envelope)
			if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionMalformed) {
				t.Fatalf("malformed decision accepted or misreported: %v", err)
			}
		})
	}
}

// Outside production the reference stays a note: the money-path fixtures set
// strings like "TEST-money-path" and there is no public to admit.
func TestCanaryDisableDecisionIsAnArtifactOnlyInProduction(t *testing.T) {
	now := time.Now().UTC()
	// Resolving the build identity loads the price board; a development stack must
	// not be dragged through that to answer a question about production.
	neverBuilt := func() ControlBuildInfo {
		t.Error("build identity was resolved outside production")
		return ControlBuildInfo{}
	}
	if err := resolveCanaryDisableDecision("TEST-money-path", false, now, neverBuilt); err != nil {
		t.Fatalf("development reference was refused: %v", err)
	}
	if err := resolveCanaryDisableDecision("", false, now, neverBuilt); !errors.Is(err, errCanaryDecisionMissing) {
		t.Fatalf("absent reference must still fail closed outside production: %v", err)
	}
}

// The whole point of the artifact: a production deployment whose disable
// reference does not resolve keeps enforcing the canary rather than opening.
func TestCanaryPolicyKeepsBuyersOutWithoutAResolvableDecision(t *testing.T) {
	t.Setenv("MERC_ENV", "production")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv("MERC_CANARY_DISABLE_DECISION_REF", "yes")
	p := loadCanaryPolicyFromEnv()
	if !p.Enabled || p.configError == nil {
		t.Fatalf("bare production reference opened admission: enabled=%v err=%v", p.Enabled, p.configError)
	}
	if p.allowsBuyerEmail("anyone@example.test") {
		t.Fatal("misconfigured policy admitted a buyer")
	}
	if err := p.validateJobShape(jobSubmit{}); err == nil {
		t.Fatal("misconfigured policy accepted a job")
	}
}

// Production configuration must CARRY both variables, empty. A deployment that
// never receives them cannot be handed a decision without also being rebuilt,
// and a placeholder value would be a decision nobody took.
func TestProductionComposeCarriesTheCanaryDisableDecision(t *testing.T) {
	compose, err := os.ReadFile("../docker-compose.prod.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{canaryDisableDecisionEnv, canaryDisableDecisionDigestEnv} {
		line := ""
		for _, candidate := range strings.Split(string(compose), "\n") {
			if strings.HasPrefix(strings.TrimSpace(candidate), name+":") {
				line = strings.TrimSpace(candidate)
			}
		}
		if line == "" {
			t.Fatalf("docker-compose.prod.yml does not pass %s to the control plane", name)
		}
		if !strings.HasSuffix(line, ":-}") {
			t.Errorf("%s is not empty by default: %s", name, line)
		}
	}
}

// A deployment that cannot admit buyers must still be observable and contactable.
func TestReadyzNamesCanaryMisconfigurationAndProbesStayReachable(t *testing.T) {
	t.Setenv("MERC_CANARY_MODE", "true") // enabled with no allowlists: configError
	securityTxt := filepath.Join(t.TempDir(), "security.txt")
	if err := os.WriteFile(securityTxt, []byte("Contact: mailto:security@example.test\n"), 0o600); err != nil {
		t.Fatalf("write security.txt: %v", err)
	}
	t.Setenv("SECURITY_TXT_PATH", securityTxt)

	routes := NewServer(nil, nil, nil, nil).Routes()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503 while canary policy is incomplete", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /readyz: %v", err)
	}
	if body["reason_code"] != readyzReasonCanaryUnconfigured {
		t.Fatalf("/readyz reason_code = %v, want %q", body["reason_code"], readyzReasonCanaryUnconfigured)
	}
	if body["reason"] == "" || body["reason"] == nil {
		t.Fatal("/readyz dropped the human-readable reason")
	}

	for _, path := range []string{"/healthz", "/version", "/.well-known/security.txt"} {
		if code := get(path).Code; code != http.StatusOK {
			t.Fatalf("%s = %d while buyer admission is closed, want 200", path, code)
		}
	}
}
