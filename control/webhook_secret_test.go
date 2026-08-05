package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Outbound webhook signing had no test at all. These secrets authenticate merc
// to buyer endpoints, so a silent failure here lets a forged event be accepted
// as ours, or ships a secret to the database in cleartext.

func TestNewWebhookSigningSecretRefusesWithoutATokenKey(t *testing.T) {
	t.Setenv("MERC_TOKEN_KEY", "")
	if _, _, err := newWebhookSigningSecret(); !errors.Is(err, errWebhookSigningKeyUnavailable) {
		t.Fatalf("want errWebhookSigningKeyUnavailable, got %v", err)
	}
}

// The sealed form must never be the plaintext: that is the difference between a
// secret at rest and a secret printed into Postgres.
func TestNewWebhookSigningSecretSealsAndRoundTrips(t *testing.T) {
	t.Setenv("MERC_TOKEN_KEY", "webhook-secret-test-key-with-at-least-32-bytes")

	plaintext, sealed, err := newWebhookSigningSecret()
	mustf(t, err, "minting: %v")
	if !strings.HasPrefix(plaintext, webhookSigningSecretPrefix) {
		t.Fatalf("plaintext lost its %q prefix: %q", webhookSigningSecretPrefix, plaintext)
	}
	if len(plaintext) <= len(webhookSigningSecretPrefix) {
		t.Fatal("plaintext carries no entropy beyond its prefix")
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("sealed value is not encrypted: %q", sealed)
	}
	if strings.Contains(sealed, plaintext) {
		t.Fatal("sealed value contains the plaintext secret verbatim")
	}

	opened, err := openWebhookSigningSecret(sealed)
	mustf(t, err, "opening a value we just sealed: %v")
	if opened != plaintext {
		t.Fatalf("round trip changed the secret: %q -> %q", plaintext, opened)
	}
}

func TestNewWebhookSigningSecretIsUniquePerCall(t *testing.T) {
	t.Setenv("MERC_TOKEN_KEY", "webhook-secret-test-key-with-at-least-32-bytes")
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		plaintext, _, err := newWebhookSigningSecret()
		mustf(t, err, "minting: %v")
		if seen[plaintext] {
			t.Fatalf("secret repeated after %d mints", i)
		}
		seen[plaintext] = true
	}
}

// Anything not produced by the sealer must be refused rather than passed
// through as if it were a usable secret.
func TestOpenWebhookSigningSecretRejectsMalformedInput(t *testing.T) {
	t.Setenv("MERC_TOKEN_KEY", "webhook-secret-test-key-with-at-least-32-bytes")
	for _, tc := range []struct{ name, sealed string }{
		{"empty", ""},
		{"unsealed plaintext", webhookSigningSecretPrefix + "abc123"},
		{"plain-prefixed legacy value", "plain:" + webhookSigningSecretPrefix + "abc123"},
		{"enc marker with garbage payload", "enc:not-base64-!!!"},
		{"sealed value of the wrong shape", sealToken("some_other_secret_entirely")},
		{"sealed prefix with no entropy", sealToken(webhookSigningSecretPrefix)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := openWebhookSigningSecret(tc.sealed); err == nil {
				t.Fatalf("accepted malformed sealed secret %q", tc.sealed)
			}
		})
	}
}

// The signature format is a wire contract with buyer endpoints. Pin it, and pin
// that it is an HMAC over "timestamp.body" rather than the body alone -- signing
// the body alone would let an old signature be replayed forever.
func TestSignWebhookProducesTimestampBoundHMAC(t *testing.T) {
	const secret = "cx_whsec_fixture"
	body := []byte(`{"event":"job.completed"}`)
	at := time.Unix(1700000000, 0)

	got := signWebhookAt(secret, body, at)

	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	want := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signature format changed:\n got %s\nwant %s", got, want)
	}

	// A different timestamp must produce a different signature, or the
	// timestamp is decorative and replay protection is not real.
	if other := signWebhookAt(secret, body, at.Add(time.Second)); other == got {
		t.Fatal("signature is independent of the timestamp; replay protection is not real")
	}
	// A different body must too.
	if other := signWebhookAt(secret, []byte(`{"event":"job.failed"}`), at); other == got {
		t.Fatal("signature is independent of the body")
	}
	// A different secret must too.
	if other := signWebhookAt("cx_whsec_other", body, at); other == got {
		t.Fatal("signature is independent of the secret")
	}
}

func TestSignWebhookUsesCurrentTime(t *testing.T) {
	before := time.Now().Unix()
	sig := signWebhook("cx_whsec_fixture", []byte("{}"))
	after := time.Now().Unix()

	rest, ok := strings.CutPrefix(sig, "t=")
	if !ok {
		t.Fatalf("signature missing timestamp field: %s", sig)
	}
	tsText, _, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("signature missing v1 field: %s", sig)
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		t.Fatalf("timestamp is not an integer: %q", tsText)
	}
	if ts < before || ts > after {
		t.Fatalf("timestamp %d outside [%d,%d]", ts, before, after)
	}
}
