package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestInlineJobBodyLimitFitsDecoderMemoryBoundary(t *testing.T) {
	if maxJobSubmitBodyBytes > 32<<20 {
		t.Fatalf("inline job JSON cap = %d, exceeds the audited 32 MiB decoder boundary", maxJobSubmitBodyBytes)
	}
	if maxRequestBodyBytes < (64<<20)+(1<<20) {
		t.Fatalf("ordinary body cap = %d, cannot carry the supported 64 MiB multipart upload", maxRequestBodyBytes)
	}
	if maxJobSubmitBodyBytes >= maxRequestBodyBytes {
		t.Fatalf("inline decoded JSON cap %d must stay below ordinary streaming/upload cap %d", maxJobSubmitBodyBytes, maxRequestBodyBytes)
	}
	if maxJSONRequestBodyBytes > 4<<20 {
		t.Fatalf("ordinary JSON cap = %d, exceeds the audited 4 MiB decoder boundary", maxJSONRequestBodyBytes)
	}
	if maxJSONRequestBodyBytes >= maxJobSubmitBodyBytes {
		t.Fatalf("ordinary JSON cap %d must stay below explicit inline-job cap %d", maxJSONRequestBodyBytes, maxJobSubmitBodyBytes)
	}

	job := httptest.NewRequest(http.MethodPost, "/v1/jobs", nil)
	if got := requestBodyLimit(job); got != maxJobSubmitBodyBytes {
		t.Fatalf("POST /v1/jobs cap = %d, want %d", got, maxJobSubmitBodyBytes)
	}
	upload := httptest.NewRequest(http.MethodPost, "/v1/files", nil)
	if got := requestBodyLimit(upload); got != maxRequestBodyBytes {
		t.Fatalf("POST /v1/files cap = %d, want %d", got, maxRequestBodyBytes)
	}
	ordinary := httptest.NewRequest(http.MethodPost, "/v1/batches", nil)
	if got := requestBodyLimit(ordinary); got != maxJSONRequestBodyBytes {
		t.Fatalf("ordinary JSON cap = %d, want %d", got, maxJSONRequestBodyBytes)
	}
}

func TestSynchronousInputReadIsBoundedAndAlwaysClosed(t *testing.T) {
	exact := &closeTrackingReader{Reader: strings.NewReader("1234")}
	got, err := readAndCloseBounded(exact, 4)
	if err != nil || string(got) != "1234" || !exact.closed {
		t.Fatalf("exact-limit read: got=%q err=%v closed=%v", got, err, exact.closed)
	}

	over := &closeTrackingReader{Reader: strings.NewReader("12345")}
	got, err = readAndCloseBounded(over, 4)
	if got != nil || !errors.Is(err, errSynchronousInputTooLarge) || !over.closed {
		t.Fatalf("overflow read: got=%q err=%v closed=%v", got, err, over.closed)
	}

	broken := &closeTrackingReader{Reader: io.MultiReader(strings.NewReader("12"), errReader{errors.New("boom")})}
	if _, err := readAndCloseBounded(broken, 4); err == nil || !broken.closed {
		t.Fatalf("read error was not surfaced/closed: err=%v closed=%v", err, broken.closed)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
