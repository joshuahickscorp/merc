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
	mustf(t, err, "encode decision: %v")
	path := filepath.Join(t.TempDir(), "canary-disable-decision.json")
	mustf(t, os.WriteFile(path, raw, 0o600), "write decision: %v")
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
		mustf(t, resolveCanaryDisableDecision(path, true, now, testCleanBuild), "governed decision was refused: %v")
	})

	t.Run("open_ended_valid", func(t *testing.T) {
		envelope := validTestDecision()
		envelope.Decision.ExpiresAt = ""
		path := writeTestDecision(t, envelope)
		mustf(t, resolveCanaryDisableDecision(path, true, now, testCleanBuild), "decision without an expiry was refused: %v")
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
		mustf(t, err, "encode tampered decision: %v")
		// The declared digest still describes the bytes that were approved.
		mustf(t, os.WriteFile(path, raw, 0o600), "rewrite decision: %v")
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
	mustf(t, resolveCanaryDisableDecision("TEST-money-path", false, now, neverBuilt), "development reference was refused: %v")
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
	must(t, err)
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
	// The env alone is a container path pointing at nothing. Without the bind
	// mount that carries the host artifact to that path, every variable above can
	// be set correctly and production still refuses with "cannot be read".
	if !strings.Contains(string(compose),
		"${MERC_CANARY_DISABLE_DECISION_SOURCE:-/dev/null}:/run/secrets/merc-canary-disable-decision.json:ro") {
		t.Fatal("docker-compose.prod.yml does not mount the decision artifact the env names")
	}
	// The refusal is on the file mode, and the operator's editor writes 0644, so
	// the required mode has to be stated where the mount is declared.
	if !strings.Contains(string(compose), "0600") {
		t.Error("docker-compose.prod.yml does not tell the operator the required file mode")
	}
}

// A bind-mounted host file at Docker's default 0644 is the likeliest operator
// mistake in this handoff, and it must be refused rather than read: anything that
// can write a world-readable decision can open buyer admission.
func TestCanaryDecisionRefusesLooselyPermissionedArtifact(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	for name, mode := range map[string]os.FileMode{
		"world_readable": 0o644,
		"group_writable": 0o660,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTestDecision(t, validTestDecision())
			mustf(t, os.Chmod(path, mode), "chmod decision: %v")
			if err := resolveCanaryDisableDecision(path, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionUnreadable) {
				t.Fatalf("decision at mode %o was accepted: %v", mode, err)
			}
		})
	}
	t.Run("owner_and_group_read", func(t *testing.T) {
		path := writeTestDecision(t, validTestDecision())
		mustf(t, os.Chmod(path, 0o640), "chmod decision: %v")
		mustf(t, resolveCanaryDisableDecision(path, true, now, testCleanBuild), "decision at mode 0640 was refused: %v")
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := resolveCanaryDisableDecision(dir, true, now, testCleanBuild); !errors.Is(err, errCanaryDecisionUnreadable) {
			t.Fatalf("a directory was accepted as a decision: %v", err)
		}
	})
}

// The decision is re-read per call on the paths that touch payouts, so it can
// stop resolving on a running process. Every one of those paths must come back
// closed, mid-flight, without a restart: a held payout that auto-released
// because the artifact went missing is money that left on no authority at all.
func TestMoneyPathClosesWhenTheDisableDecisionStopsResolving(t *testing.T) {
	t.Setenv("MERC_ENV", "development")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv(canaryDisableDecisionEnv, "INC-canary-exit-1")

	gate, err := canaryManualPayoutGate()
	if err != nil || gate {
		t.Fatalf("resolved decision: manual payout gate=%v err=%v, want off", gate, err)
	}
	if limit, err := canaryRetryLimit(); err != nil || limit != maxTaskRetries {
		t.Fatalf("resolved decision: retry limit=%d err=%v, want the platform ceiling", limit, err)
	}
	if limit, err := canaryArtifactLimit(1 << 20); err != nil || limit != 1<<20 {
		t.Fatalf("resolved decision: artifact limit=%d err=%v, want the computed value", limit, err)
	}

	// The artifact stops resolving under a process that is already serving.
	os.Unsetenv(canaryDisableDecisionEnv)

	gate, err = canaryManualPayoutGate()
	if !gate || !errors.Is(err, errCanaryDecisionMissing) {
		t.Fatalf("revoked decision: manual payout gate=%v err=%v, want the gate on and the refusal named", gate, err)
	}
	if _, err := canaryRetryLimit(); !errors.Is(err, errCanaryDecisionMissing) {
		t.Fatalf("revoked decision: retry limit did not refuse: %v", err)
	}
	if _, err := canaryArtifactLimit(1 << 20); !errors.Is(err, errCanaryDecisionMissing) {
		t.Fatalf("revoked decision: artifact limit did not refuse: %v", err)
	}
}

// Halting payouts is defensible; halting them where nobody is looking is not.
// s.canary is captured at boot, so a decision that stops resolving afterwards
// leaves admission open and would leave the probe green: the readiness answer has
// to come from the same read the money path makes.
func TestReadyzGoesRedWhenTheDisableDecisionStopsResolving(t *testing.T) {
	t.Setenv("MERC_ENV", "development")
	t.Setenv("MERC_CANARY_MODE", "false")
	t.Setenv(canaryDisableDecisionEnv, "INC-canary-exit-1")
	// Reached only if the canary check passes, and it fails before the nil store
	// is touched, so a regression reports a wrong reason_code instead of panicking.
	t.Setenv("MERC_PAYMENT_MODE", "sealed")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_readyz_probe")

	server := NewServer(nil, nil, nil, nil)
	if server.canary.Enabled {
		t.Fatalf("decision did not resolve at boot: %v", server.canary.configError)
	}
	routes := server.Routes()
	probe := func() map[string]any {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz = %d, want 503", rec.Code)
		}
		var body map[string]any
		mustf(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode /readyz: %v")
		return body
	}
	if code := probe()["reason_code"]; code != readyzReasonPaymentInvalid {
		t.Fatalf("with the decision in force /readyz blamed %v, want %q", code, readyzReasonPaymentInvalid)
	}

	os.Unsetenv(canaryDisableDecisionEnv)
	if code := probe()["reason_code"]; code != readyzReasonCanaryUnconfigured {
		t.Fatalf("/readyz reason_code = %v after the decision stopped resolving, want %q",
			code, readyzReasonCanaryUnconfigured)
	}
}

// A deployment that cannot admit buyers must still be observable and contactable.
func TestReadyzNamesCanaryMisconfigurationAndProbesStayReachable(t *testing.T) {
	t.Setenv("MERC_CANARY_MODE", "true") // enabled with no allowlists: configError
	securityTxt := filepath.Join(t.TempDir(), "security.txt")
	mustf(t, os.WriteFile(securityTxt, []byte("Contact: mailto:security@example.test\n"), 0o600), "write security.txt: %v")
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
	mustf(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode /readyz: %v")
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

// Readiness must answer from BOTH copies of the policy.
//
// The first cut of this probe read only the boot copy, and a decision that stopped
// resolving on a running process left it green while held payouts halted. The fix
// swung to reading only the fresh copy, which opened the mirror hole: a decision
// that was unresolvable AT BOOT and is repaired afterwards without a restart turns
// the probe green while buyer admission still 403s everyone from s.canary. Both
// directions produce a superficially healthy service, which is the precise failure
// this lane exists to remove, so readiness checks both.
func TestReadyzRefusesWhenEitherCopyOfTheCanaryPolicyIsBroken(t *testing.T) {
	broken := CanaryPolicy{Enabled: true, configError: errors.New("no decision artifact")}

	for name, tc := range map[string]struct {
		boot      CanaryPolicy
		freshRef  string
		wantReady bool
	}{
		"boot broken, environment repaired since": {
			boot: broken, freshRef: "", wantReady: false,
		},
		"boot fine, environment broken now": {
			boot: CanaryPolicy{}, freshRef: "bare-string-not-an-artifact", wantReady: false,
		},
		"both fine": {boot: CanaryPolicy{}, freshRef: "", wantReady: true},
	} {
		t.Run(name, func(t *testing.T) {
			// The fresh read only refuses a bare reference in production.
			if tc.freshRef != "" {
				t.Setenv("MERC_ENV", "production")
				t.Setenv("MERC_CANARY_MODE", "false")
				t.Setenv(canaryDisableDecisionEnv, tc.freshRef)
			}
			// A real store, because the probe checks the database after the
			// canary and the control case has to get that far to prove it passed
			// both canary checks rather than tripping over the first one.
			_, store, _ := openActivationStore(t)
			srv := &Server{canary: tc.boot, store: store}
			recorder := httptest.NewRecorder()
			srv.handleReadyz(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			var body map[string]any
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			ready := recorder.Code == http.StatusOK

			// A readiness probe can legitimately refuse for reasons other than the
			// canary (no database, for instance), so a refusal only counts here
			// when it names the canary.
			canaryRefusal := body["reason_code"] == readyzReasonCanaryUnconfigured
			switch {
			case tc.wantReady && canaryRefusal:
				t.Fatalf("readiness blamed the canary with both copies healthy: %v", body)
			case !tc.wantReady && ready:
				t.Fatalf("readiness reported OK with a broken canary policy (%s)", name)
			case !tc.wantReady && !canaryRefusal:
				t.Fatalf("readiness refused for %q rather than naming the canary: %v",
					body["reason_code"], body)
			}
			if !tc.wantReady {
				t.Logf("%s -> %v", name, body["reason"])
			}
		})
	}
}
