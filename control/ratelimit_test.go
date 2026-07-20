package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIPResistsXFFSpoofing(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("single-hop XFF: want 203.0.113.9, got %q", got)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if got := clientIP(r2); got != "203.0.113.9" {
		t.Fatalf("spoofed multi-hop XFF: want the real last hop 203.0.113.9, got %q (attacker's 6.6.6.6 must be ignored)", got)
	}

	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3, 203.0.113.9")
	if got := clientIP(r3); got != "203.0.113.9" {
		t.Fatalf("multiple spoofed hops: want the real last hop 203.0.113.9, got %q", got)
	}

	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	r4.Header.Set("X-Real-IP", "198.51.100.7")
	if got := clientIP(r4); got != "198.51.100.7" {
		t.Fatalf("X-Real-IP fallback: want 198.51.100.7, got %q", got)
	}
}

func TestIsRemoteTrustsXFFLoopbackClaimUnconditionally(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if isRemote(r) {
		t.Fatal("expected isRemote to follow the (spoofable) XFF claim of loopback  -  if this now returns true, the code changed and docs/SECURITY.md's gap note is stale and should be updated")
	}
}
