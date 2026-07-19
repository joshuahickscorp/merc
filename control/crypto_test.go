package main

import (
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
)

func TestSealOpenRoundTrip(t *testing.T) {
	t.Setenv("CX_TOKEN_KEY", "test-secret-key")
	sealed := sealToken("ghp_secrettoken123")
	if sealed == "ghp_secrettoken123" || len(sealed) < 5 || sealed[:4] != "enc:" {
		t.Fatalf("token not sealed: %q", sealed)
	}
	if got := openToken(sealed); got != "ghp_secrettoken123" {
		t.Fatalf("open = %q, want original", got)
	}
}

func TestSealNoKeyIsHonestPlaintext(t *testing.T) {
	t.Setenv("CX_TOKEN_KEY", "")
	sealed := sealToken("abc")
	if sealed != "plain:abc" {
		t.Fatalf("expected plain: marker, got %q", sealed)
	}
	if got := openToken(sealed); got != "abc" {
		t.Fatalf("open = %q", got)
	}
}

func TestSealConfiguredKeyFailsClosedOnEntropyFailure(t *testing.T) {
	key := make([]byte, 32)
	sealed := sealTokenWithReader("must-not-leak", key, failingSealReader{})
	if sealed != "" {
		t.Fatalf("entropy failure produced stored value %q; want fail-closed empty result", sealed)
	}
}

type failingSealReader struct{}

func (failingSealReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestRedundancySelectionHashIsDeterministicNotOrdinal(t *testing.T) {
	jobID := uuid.New()
	tasks := make([]uuid.UUID, 20)
	for i := range tasks {
		tasks[i] = uuid.New()
	}

	for _, id := range tasks {
		first := redundancySelectionHash(jobID, id)
		second := redundancySelectionHash(jobID, id)
		if first != second {
			t.Fatalf("redundancySelectionHash not deterministic for task %s", id)
		}
	}

	sorted := append([]uuid.UUID(nil), tasks...)
	sort.Slice(sorted, func(i, j int) bool {
		return redundancySelectionHash(jobID, sorted[i]) < redundancySelectionHash(jobID, sorted[j])
	})
	same := true
	for i := range tasks {
		if tasks[i] != sorted[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("hash-sorted order matched original ordinal order  -  selection is not actually keyed by the hash")
	}

	otherJob := uuid.New()
	differs := false
	for _, id := range tasks {
		if redundancySelectionHash(jobID, id) != redundancySelectionHash(otherJob, id) {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("redundancySelectionHash did not vary with jobID")
	}
}
