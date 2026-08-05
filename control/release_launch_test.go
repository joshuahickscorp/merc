package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLaunchSecretsRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".merc-launch.env")
	must(t, os.WriteFile(path, []byte("MERC_TOKEN_KEY=one\nMERC_TOKEN_KEY=two\n"), 0o600))
	if _, err := loadLaunchSecrets(path); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate secret file error=%v", err)
	}
	must(t, os.WriteFile(path, []byte("MERC_TOKEN_KEY=one\n"), 0o644))
	must(t, os.Chmod(path, 0o644))
	if _, err := loadLaunchSecrets(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unsafe mode error=%v", err)
	}
}

func TestLoadLaunchConfigRejectsLevelC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "level-c.yaml")
	must(t, os.WriteFile(path, []byte("schema_version: 1\nenvironment: production\n"), 0o600))
	if _, _, err := loadLaunchConfig(path, "production"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("Level C config error=%v", err)
	}
}

func TestLoadLaunchConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.yaml")
	must(t, os.WriteFile(path, []byte("schema_version: 1\nenvironment: staging\nunreviewed: true\n"), 0o600))
	if _, _, err := loadLaunchConfig(path, "staging"); err == nil || !strings.Contains(err.Error(), "field") {
		t.Fatalf("unknown config field error=%v", err)
	}
}

func TestIdentitySecretFingerprintsDetectContinuityDrift(t *testing.T) {
	first, err := identitySecretFingerprints(map[string]string{"MERC_TOKEN_KEY": "one", "MERC_VERIFICATION_SAMPLE_SECRET": "two"})
	must(t, err)
	second, err := identitySecretFingerprints(map[string]string{"MERC_TOKEN_KEY": "one", "MERC_VERIFICATION_SAMPLE_SECRET": "changed"})
	must(t, err)
	if equalStringMap(first, second) || first["MERC_VERIFICATION_SAMPLE_SECRET"] == second["MERC_VERIFICATION_SAMPLE_SECRET"] {
		t.Fatal("identity-critical secret continuity drift was not detected")
	}
}

func TestScrubbedReleaseEnvDropsPaymentAndLaunchCredentials(t *testing.T) {
	got := strings.Join(scrubbedReleaseEnv([]string{"PATH=/bin", "STRIPE_SECRET_KEY=sk_live_never", "MERC_TOKEN_KEY=never", "AWS_SECRET_ACCESS_KEY=never"}), "\n")
	if got != "PATH=/bin" {
		t.Fatalf("scrubbed environment=%q", got)
	}
}

func TestLaunchStateIsPrivateAndRoundTrips(t *testing.T) {
	root := t.TempDir()
	want := launchState{SchemaVersion: 2, Status: "planned", Plan: launchPlan{PlanSHA256: "abc", IdentityFingerprints: map[string]string{"MERC_TOKEN_KEY": "fingerprint"}}}
	must(t, writeLaunchState(root, want))
	info, err := os.Stat(releaseStatePath(root))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%#o, want 0600", info.Mode().Perm())
	}
	got, err := readLaunchState(root)
	must(t, err)
	if got.Status != want.Status || got.Plan.PlanSHA256 != want.Plan.PlanSHA256 || !equalStringMap(got.Plan.IdentityFingerprints, want.Plan.IdentityFingerprints) {
		t.Fatalf("state roundtrip got=%+v want=%+v", got, want)
	}
}

func TestAdapterEnvironmentIsShellQuotedAndPrivate(t *testing.T) {
	root := t.TempDir()
	path, cleanup, err := writeAdapterEnv(root, map[string]string{"STRIPE_SECRET_KEY": "sk_test_$literal'quote"})
	must(t, err)
	defer cleanup()
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("adapter env mode=%#o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	if got, want := string(raw), "STRIPE_SECRET_KEY='sk_test_$literal'\"'\"'quote'\n"; got != want {
		t.Fatalf("adapter env=%q want %q", got, want)
	}
}

func TestReleaseStateTransitionsAreFailClosed(t *testing.T) {
	if !operationAllowed("planned", "apply") || operationAllowed("planned", "canary") {
		t.Fatal("planned release operation guard is unsafe")
	}
	if !operationAllowed("canary_complete", "soak") || operationAllowed("soak_complete", "soak") {
		t.Fatal("soak operation guard is unsafe")
	}
	for _, tc := range []struct{ status, want string }{
		{"planned", "apply"}, {"apply_failed", "apply"}, {"applied", "rollback"},
		{"rollback_rehearsed", "canary"}, {"canary_complete", "soak"},
	} {
		got, err := resumeOperation(tc.status)
		if err != nil || got != tc.want {
			t.Fatalf("resume %s got %q err=%v want %q", tc.status, got, err, tc.want)
		}
	}
	if _, err := resumeOperation("deploy-candidate_running"); err == nil {
		t.Fatal("ambiguous interrupted adapter was made resumable")
	}
	if _, err := resumeOperation("destroy_failed"); err == nil {
		t.Fatal("failed teardown was made implicitly resumable")
	}
}

func TestReleaseAdaptersUseAuditedScriptsOnly(t *testing.T) {
	adapters, err := releaseAdapters("launch")
	must(t, err)
	if len(adapters) != 5 || adapters[0].Name != "deploy-candidate" || adapters[4].Name != "qualifying-soak" {
		t.Fatalf("launch adapters=%+v", adapters)
	}
	if script, err := adapterScript(adapters[2].Name); err != nil || script != "go-closure-restart-storm.sh" {
		t.Fatalf("restart adapter script=%q err=%v", script, err)
	}
	destroy, err := releaseAdapters("destroy")
	if err != nil || len(destroy) != 1 || destroy[0].Name != "controlled-teardown" {
		t.Fatalf("destroy adapter=%+v err=%v", destroy, err)
	}
}

func TestRemoteProfileDigestBindsEveryDeclaredInput(t *testing.T) {
	root := filepath.Clean(filepath.Join(".."))
	values := map[string]string{
		"MERC_TOKEN_KEY": "first", "STAGING_TLS_HOSTNAME": "staging.example.test",
	}
	first, err := remoteProfileDigest(root, values)
	must(t, err)
	repeated, err := remoteProfileDigest(root, map[string]string{
		"STAGING_TLS_HOSTNAME": "staging.example.test", "MERC_TOKEN_KEY": "first",
	})
	must(t, err)
	changed, err := remoteProfileDigest(root, map[string]string{
		"MERC_TOKEN_KEY": "changed", "STAGING_TLS_HOSTNAME": "staging.example.test",
	})
	must(t, err)
	if first != repeated || first == changed || len(first) != 64 {
		t.Fatalf("remote profile continuity first=%s repeated=%s changed=%s", first, repeated, changed)
	}
	contract, err := loadLaunchInputContract(root)
	must(t, err)
	env := append([]string{}, os.Environ()...)
	env = append(env, "MERC_RELEASE_IDENTITY_PROFILE_SELF_TEST=1")
	for _, input := range contract.Inputs {
		env = append(env, input.Name+"="+values[input.Name])
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "go-closure-release-identity.sh"))
	cmd.Env = env
	got, err := cmd.Output()
	mustf(t, err, "shell profile: %v")
	if strings.TrimSpace(string(got)) != first {
		t.Fatalf("shell/go profile mismatch got=%q want=%q", got, first)
	}
}

func TestAdapterReceiptsAreExactAndRemainUnderStagingEvidence(t *testing.T) {
	got, err := extractAdapterReceipt("/srv/merc", []byte("go-closure: PASS receipt: /srv/merc/evidence/go-closure/deploy.json\n"))
	if err != nil || got != "evidence/go-closure/deploy.json" {
		t.Fatalf("receipt got=%q err=%v", got, err)
	}
	for _, output := range [][]byte{
		[]byte("go-closure: PASS receipt: /tmp/outside.json\n"),
		[]byte("go-closure: PASS receipt: /srv/merc/evidence/go-closure/one.json\ngo-closure: PASS receipt: /srv/merc/evidence/go-closure/two.json\n"),
		[]byte("no receipt\n"),
	} {
		if _, err := extractAdapterReceipt("/srv/merc", output); err == nil {
			t.Fatalf("unsafe receipt output accepted: %q", output)
		}
	}
}

func TestRootEvidenceIsPrivateAndStateBound(t *testing.T) {
	root := t.TempDir()
	evidence := releaseRootEvidence{SchemaVersion: 1, Kind: "merc_level_b_release_root_evidence", Status: "PASS",
		PlanSHA256: "plan", CandidateCommit: "commit", RemoteProfileSHA256: "profile",
		Receipts: map[string]string{"deploy": "evidence/go-closure/deploy.json"}, EvidenceChain: json.RawMessage(`{"status":"PASS"}`)}
	must(t, writeReleaseEvidence(root, evidence))
	info, err := os.Stat(releaseEvidencePath(root))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("root evidence mode=%#o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(releaseEvidencePath(root))
	if err != nil || strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("root evidence read err=%v raw=%q", err, raw)
	}
}

func TestLaunchPlanIncludesOnlyIdentityFingerprintDigests(t *testing.T) {
	values := map[string]string{"MERC_TOKEN_KEY": "super-secret", "MERC_VERIFICATION_SAMPLE_SECRET": "different-secret"}
	fingerprints, err := identitySecretFingerprints(values)
	must(t, err)
	raw, err := canonicalProofJSON(fingerprints)
	must(t, err)
	if strings.Contains(string(raw), "super-secret") || strings.Contains(string(raw), "different-secret") {
		t.Fatalf("secret values leaked into release fingerprint output: %s", raw)
	}
}

func TestBuildLaunchInputsReportsMissingContractEntries(t *testing.T) {
	root := filepath.Clean(filepath.Join(".."))
	inputs, err := buildLaunchInputs(root, map[string]string{})
	must(t, err)
	if inputs.Ready || len(inputs.Missing) < 8 {
		t.Fatalf("missing input contract=%+v", inputs)
	}
	for _, missing := range inputs.Missing {
		if missing.Name == "STRIPE_SECRET_KEY" && !missing.Secret {
			t.Fatal("Stripe secret classification lost")
		}
	}
}
