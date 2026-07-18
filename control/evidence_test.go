package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalJSONSortedCompactNoHTMLEscape(t *testing.T) {
	got, err := canonicalProofJSON(map[string]any{"b": 1, "a": "x<y", "c": true})
	if err != nil {
		t.Fatal(err)
	}
	// keys sorted, compact separators, and < NOT escaped (Python json.dumps parity)
	want := `{"a":"x<y","b":1,"c":true}`
	if string(got) != want {
		t.Errorf("canonicalProofJSON = %q, want %q", got, want)
	}
}

func TestFramedDomainSeparation(t *testing.T) {
	// framed("ab")+framed("c") must differ from framed("a")+framed("bc")
	h1 := sha256.New()
	framed(h1, []byte("ab"))
	framed(h1, []byte("c"))
	h2 := sha256.New()
	framed(h2, []byte("a"))
	framed(h2, []byte("bc"))
	if hex.EncodeToString(h1.Sum(nil)) == hex.EncodeToString(h2.Sum(nil)) {
		t.Error("framed() failed to separate distinct field boundaries")
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := atomicWrite(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "hello" {
		t.Fatalf("atomicWrite content = %q err %v", b, err)
	}
	// no leftover temp files
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".cx-tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestSourceFingerprintDeterministicAndFieldParity(t *testing.T) {
	// Runs against this repo (a git tree). Assert determinism + that the --field
	// values agree with the JSON object (the property the CI contract relies on).
	root, err := gitBytes(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skip("not a git tree")
	}
	r := strings.TrimSpace(string(root))
	a, err := sourceFingerprint(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sourceFingerprint(r)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceSHA256 != b.SourceSHA256 || a.StatusSHA256 != b.StatusSHA256 {
		t.Error("source fingerprint is not deterministic")
	}
	if len(a.SourceSHA256) != 64 || len(a.StatusSHA256) != 64 {
		t.Errorf("digests not sha256 hex: %q %q", a.SourceSHA256, a.StatusSHA256)
	}
	if a.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", a.SchemaVersion)
	}
	// toMap round-trips through canonicalProofJSON with sorted keys
	j, _ := canonicalProofJSON(a.toMap())
	var back map[string]any
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatalf("canonical json invalid: %v", err)
	}
	if back["head"] != a.Head {
		t.Errorf("head field mismatch: %v vs %v", back["head"], a.Head)
	}
}
