package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestClientIPResistsXFFSpoofing(t *testing.T) {
	t.Setenv("CX_TRUSTED_PROXY_CIDRS", "")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.20:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r); got != "198.51.100.20" {
		t.Fatalf("untrusted peer must ignore XFF: got %q", got)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "172.20.0.2:4321"
	r2.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	t.Setenv("CX_TRUSTED_PROXY_CIDRS", "172.16.0.0/12")
	if got := clientIP(r2); got != "203.0.113.9" {
		t.Fatalf("trusted normalized XFF: want 203.0.113.9, got %q", got)
	}

	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.RemoteAddr = "203.0.113.10:1234"
	r3.Header.Set("X-Forwarded-For", "127.0.0.1")
	if got := clientIP(r3); got != "203.0.113.10" {
		t.Fatalf("untrusted loopback claim bypassed peer identity: %q", got)
	}

	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	r4.RemoteAddr = "172.20.0.2:1234"
	r4.Header.Set("X-Real-IP", "198.51.100.7")
	if got := clientIP(r4); got != "198.51.100.7" {
		t.Fatalf("X-Real-IP fallback: want 198.51.100.7, got %q", got)
	}
}

func TestIsRemoteRejectsUntrustedXFFLoopbackClaim(t *testing.T) {
	_ = os.Unsetenv("CX_TRUSTED_PROXY_CIDRS")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if !isRemote(r) {
		t.Fatal("untrusted forwarding header bypassed remote-client classification")
	}
}
