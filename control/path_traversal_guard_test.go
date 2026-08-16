package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectPathTraversalBlocksDotDotBeforeMux(t *testing.T) {
	handler := NewServer(nil, nil, nil, nil).Routes()

	for _, path := range []string{
		"/assets/site/../../control/schema.sql",
		"/assets/site/../../v1/me",
		"/assets/site/foo/../../../etc/passwd",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:9"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTemporaryRedirect || rec.Code == http.StatusMovedPermanently {
			t.Fatalf("%s: mux bounced the request to %q (HTTP %d); parent segments must be refused first",
				path, rec.Header().Get("Location"), rec.Code)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: HTTP %d body=%s, want 404", path, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("%s: unexpected Location %q", path, loc)
		}
		if strings.Contains(rec.Body.String(), "CREATE TABLE") {
			t.Fatalf("%s leaked schema contents", path)
		}
	}

	// A clean public asset path still reaches the asset handler (404 if the
	// file is absent — not the traversal guard).
	req := httptest.NewRequest(http.MethodGet, "/assets/site/does-not-exist.png", nil)
	req.RemoteAddr = "127.0.0.1:9"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("clean missing asset: HTTP %d, want 404", rec.Code)
	}
}

func TestRequestPathHasParentSegment(t *testing.T) {
	if requestPathHasParentSegment(httptest.NewRequest(http.MethodGet, "/assets/site/logo.png", nil)) {
		t.Fatal("clean path treated as traversal")
	}
	req := httptest.NewRequest(http.MethodGet, "/assets/site/../../v1/me", nil)
	if !requestPathHasParentSegment(req) {
		t.Fatal("dot-dot path not detected")
	}
}
