package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBuildPinnedWebhookTransportDisablesProxyAndDialsOnlyResolved(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// Happy path: pin the real listener address; DialContext must ignore the
	// hostname argument and connect to the pre-resolved IP only.
	okTarget := resolvedWebhookTarget{
		host: "webhook.example.test",
		port: port,
		ips:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	tr := buildPinnedWebhookTransport(okTarget, webhookTargetPolicy{})
	if tr.Proxy != nil {
		t.Fatal("pinned webhook transport must set Proxy = nil so env proxies cannot bypass the SSRF guard")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := tr.DialContext(ctx, "tcp", net.JoinHostPort("webhook.example.test", port))
	if err != nil {
		t.Fatalf("dial with hostname argument should still use pre-resolved IP: %v", err)
	}
	remote := conn.RemoteAddr().String()
	_ = conn.Close()
	host, _, err := net.SplitHostPort(remote)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("remote = %q, want 127.0.0.1:<port>", remote)
	}

	// Negative path: pin an unroutable documentation address while the real
	// listener is on 127.0.0.1. DialContext must not fall back to the address
	// argument, even when that argument would succeed.
	badTarget := resolvedWebhookTarget{
		host: "127.0.0.1",
		port: port,
		ips:  []net.IP{net.ParseIP("203.0.113.50")},
	}
	trBad := buildPinnedWebhookTransport(badTarget, webhookTargetPolicy{})
	if trBad.Proxy != nil {
		t.Fatal("pinned webhook transport must set Proxy = nil")
	}
	badCtx, badCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer badCancel()
	if conn, err := trBad.DialContext(badCtx, "tcp", net.JoinHostPort("127.0.0.1", port)); err == nil {
		_ = conn.Close()
		t.Fatal("DialContext connected using the address argument instead of the pinned IP set")
	}
}

func TestWebhookPinnedTransportIgnoresHTTPProxyEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	host, _, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("test server address is not an IP: %s", host)
	}

	// Point HTTP(S)_PROXY at a blackhole. Proxy = nil must still reach srv.
	t.Setenv("HTTP_PROXY", "http://203.0.113.1:9")
	t.Setenv("HTTPS_PROXY", "http://203.0.113.1:9")
	t.Setenv("http_proxy", "http://203.0.113.1:9")
	t.Setenv("https_proxy", "http://203.0.113.1:9")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client := newWebhookHTTPClientWithPolicy(webhookTargetPolicy{
		resolver:     fixedWebhookResolver{ips: []net.IPAddr{{IP: ip}}},
		allowPrivate: true,
		allowHTTP:    true,
	})
	resp, err := client.Post(srv.URL+"/hook", "application/json", nil)
	if err != nil {
		t.Fatalf("pinned client failed despite Proxy=nil: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

type fixedWebhookResolver struct {
	ips []net.IPAddr
}

func (r fixedWebhookResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	out := make([]net.IPAddr, len(r.ips))
	copy(out, r.ips)
	return out, nil
}
