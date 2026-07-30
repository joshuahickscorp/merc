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
