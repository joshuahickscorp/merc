package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLaunchSecretsRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".merc-launch.env")
	if err := os.WriteFile(path, []byte("MERC_TOKEN_KEY=one\nMERC_TOKEN_KEY=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLaunchSecrets(path); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate secret file error=%v", err)
	}
	if err := os.WriteFile(path, []byte("MERC_TOKEN_KEY=one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLaunchSecrets(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unsafe mode error=%v", err)
	}
}

func TestLoadLaunchConfigRejectsLevelC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "level-c.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nenvironment: production\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadLaunchConfig(path, "production"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("Level C config error=%v", err)
	}
}

func TestIdentitySecretFingerprintsDetectContinuityDrift(t *testing.T) {
	first, err := identitySecretFingerprints(map[string]string{"MERC_TOKEN_KEY": "one", "MERC_VERIFICATION_SAMPLE_SECRET": "two"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := identitySecretFingerprints(map[string]string{"MERC_TOKEN_KEY": "one", "MERC_VERIFICATION_SAMPLE_SECRET": "changed"})
	if err != nil {
		t.Fatal(err)
	}
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
	want := launchState{SchemaVersion: 1, Status: "planned", Plan: launchPlan{PlanSHA256: "abc", IdentityFingerprints: map[string]string{"MERC_TOKEN_KEY": "fingerprint"}}}
	if err := writeLaunchState(root, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(releaseStatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%#o, want 0600", info.Mode().Perm())
	}
	got, err := readLaunchState(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != want.Status || got.Plan.PlanSHA256 != want.Plan.PlanSHA256 || !equalStringMap(got.Plan.IdentityFingerprints, want.Plan.IdentityFingerprints) {
		t.Fatalf("state roundtrip got=%+v want=%+v", got, want)
	}
}
