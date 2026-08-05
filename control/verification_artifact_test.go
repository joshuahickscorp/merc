package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSealedVerificationObjectKeyIsAttemptAndContentBound(t *testing.T) {
	taskID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	digest := strings.Repeat("a", 64)
	got := sealedVerificationObjectKey(taskID, 7, digest)
	want := "verification/11111111-2222-3333-4444-555555555555/attempt-7/" + digest + ".result"
	if got != want {
		t.Fatalf("sealed key = %q, want %q", got, want)
	}
	if got == sealedVerificationObjectKey(taskID, 8, digest) {
		t.Fatal("different attempts must not share a sealed object key")
	}
	if got == sealedVerificationObjectKey(taskID, 7, strings.Repeat("b", 64)) {
		t.Fatal("different content digests must not share a sealed object key")
	}
}

type countedArtifactReader struct {
	r    io.Reader
	read int
	maxP int
}

func (r *countedArtifactReader) Read(p []byte) (int, error) {
	if len(p) > r.maxP {
		r.maxP = len(p)
	}
	n, err := r.r.Read(p)
	r.read += n
	return n, err
}

func TestReadExactObjectSizeBoundedNeverReadsPastDeclaredPlusOne(t *testing.T) {
	source := bytes.Repeat([]byte("x"), 1<<20)
	r := &countedArtifactReader{r: bytes.NewReader(source)}
	_, err := readExactObjectSizeBounded(r, "staging", 8, 64)
	if !errors.Is(err, ErrVerificationArtifactChanged) {
		t.Fatalf("stale HEAD read error = %v, want %v", err, ErrVerificationArtifactChanged)
	}
	if r.read != 9 || r.maxP > 9 {
		t.Fatalf("stale HEAD consumed/requested %d/%d bytes, want exactly <= declared+1 (9)", r.read, r.maxP)
	}
}

func TestReadExactObjectSizeBoundedRejectsHeadOversizeWithoutReading(t *testing.T) {
	r := &countedArtifactReader{r: strings.NewReader("must not be read")}
	_, err := readExactObjectSizeBounded(r, "staging", 65, 64)
	if !errors.Is(err, ErrVerificationArtifactTooLarge) {
		t.Fatalf("oversize error = %v, want %v", err, ErrVerificationArtifactTooLarge)
	}
	var sizeErr *VerificationArtifactTooLargeError
	if !errors.As(err, &sizeErr) || sizeErr.ObservedBytes != 65 || sizeErr.MaxBytes != 64 {
		t.Fatalf("typed oversize evidence = %#v", sizeErr)
	}
	if r.read != 0 {
		t.Fatalf("oversized HEAD consumed %d body bytes, want 0", r.read)
	}
}

func TestReadExactObjectSizeBoundedCapPlusOneIsTypedOversize(t *testing.T) {
	r := &countedArtifactReader{r: strings.NewReader("123456789and-more")}
	_, err := readExactObjectSizeBounded(r, "staging", 8, 8)
	if !errors.Is(err, ErrVerificationArtifactTooLarge) {
		t.Fatalf("cap+1 error = %v, want %v", err, ErrVerificationArtifactTooLarge)
	}
	if r.read != 9 || r.maxP > 9 {
		t.Fatalf("cap+1 consumed/requested %d/%d bytes, want 9", r.read, r.maxP)
	}
}

func TestReadExactObjectSizeBoundedExactBody(t *testing.T) {
	r := &countedArtifactReader{r: strings.NewReader("12345678")}
	got, err := readExactObjectSizeBounded(r, "sealed", 8, 8)
	mustf(t, err, "exact bounded read: %v")
	if string(got) != "12345678" || cap(got) > 9 {
		t.Fatalf("exact bounded read = %q len/cap=%d/%d", got, len(got), cap(got))
	}
	if r.read != 8 || r.maxP > 9 {
		t.Fatalf("exact read consumed/requested %d/%d bytes", r.read, r.maxP)
	}
}
