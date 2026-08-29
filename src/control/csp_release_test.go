package main

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	cspHeaderPattern    = regexp.MustCompile(`Content-Security-Policy "([^"]+)"`)
	inlineStylePattern  = regexp.MustCompile(`(?is)<style(?:\s[^>]*)?>(.*?)</style>`)
	inlineScriptPattern = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>(.*?)</script>`)
)

// Inline scripts and styles are deliberately hash-bound in the public TLS
// policy. A page change without its corresponding Caddy hash makes the page
// fail closed at the browser; leaving an old hash behind expands the set of
// executable bytes. Keep the policy an exact manifest of shipped inline bytes.
func TestCaddyCSPHashesExactlyBindShippedInlineAssets(t *testing.T) {
	caddy, err := os.ReadFile("../../ops/deploy/Caddyfile")
	must(t, err)
	match := cspHeaderPattern.FindSubmatch(caddy)
	if len(match) != 2 {
		t.Fatal("Caddyfile has no parseable Content-Security-Policy header")
	}
	csp := string(match[1])

	for _, required := range []string{
		"default-src 'self'", "base-uri 'none'", "object-src 'none'",
		"frame-ancestors 'none'", "form-action 'self'",
	} {
		if !strings.Contains(csp, required) {
			t.Errorf("CSP no longer contains required hardening %q", required)
		}
	}
	if !strings.Contains(cspDirective(csp, "script-src"), "https://js.stripe.com") {
		t.Error("script-src must explicitly allow Stripe.js while retaining hash binding")
	}

	wantStyle, wantScript := map[string]bool{}, map[string]bool{}
	for _, page := range []string{
		"../../clients/web/index.html", "../../clients/web/admin.html", "../../clients/web/buyer.html",
		"../../clients/web/prices.html", "../../clients/web/supplier.html",
	} {
		html, err := os.ReadFile(page)
		must(t, err)
		for _, body := range inlineStylePattern.FindAllSubmatch(html, -1) {
			wantStyle[cspSHA256(body[1])] = true
		}
		for _, body := range inlineScriptPattern.FindAllSubmatch(html, -1) {
			// An empty script element with src= is governed by its source allowlist;
			// hashing the empty body here would turn a meaningless hash into an
			// accidental script allowance.
			if len(body[1]) != 0 {
				wantScript[cspSHA256(body[1])] = true
			}
		}
	}
	assertExactCSPHashes(t, "style-src", wantStyle, cspHashes(cspDirective(csp, "style-src")))
	assertExactCSPHashes(t, "script-src", wantScript, cspHashes(cspDirective(csp, "script-src")))
}

func cspSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func cspDirective(csp, name string) string {
	for _, directive := range strings.Split(csp, ";") {
		fields := strings.Fields(strings.TrimSpace(directive))
		if len(fields) != 0 && fields[0] == name {
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

func cspHashes(directive string) map[string]bool {
	hashes := map[string]bool{}
	for _, value := range strings.Fields(directive) {
		value = strings.Trim(value, "'")
		if strings.HasPrefix(value, "sha256-") {
			hashes[value] = true
		}
	}
	return hashes
}

func assertExactCSPHashes(t *testing.T, directive string, want, got map[string]bool) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("%s hashes = %v, want exact set %v", directive, got, want)
		return
	}
	for hash := range want {
		if !got[hash] {
			t.Errorf("%s does not bind shipped inline bytes %s", directive, hash)
		}
	}
}
