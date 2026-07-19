package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestReadBoundedRemoteBodyRejectsInsteadOfTruncating(t *testing.T) {
	const limit = int64(4)

	body, err := readBoundedRemoteBody(strings.NewReader("1234"), limit)
	if err != nil || string(body) != "1234" {
		t.Fatalf("exact-limit read = %q, %v", body, err)
	}
	body, err = readBoundedRemoteBody(strings.NewReader("12345"), limit)
	if !errors.Is(err, errRemoteResponseTooLarge) {
		t.Fatalf("oversized read error = %v, want %v", err, errRemoteResponseTooLarge)
	}
	if body != nil {
		t.Fatalf("oversized read returned truncated bytes %q", body)
	}
}

func TestReadBoundedRemoteBodyRejectsInvalidInputsAndPropagatesReadError(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		if _, err := readBoundedRemoteBody(strings.NewReader("x"), limit); err == nil {
			t.Fatalf("limit %d was accepted", limit)
		}
	}
	if _, err := readBoundedRemoteBody(nil, 1); err == nil {
		t.Fatal("nil response body was accepted")
	}
	want := errors.New("upstream read failed")
	if _, err := readBoundedRemoteBody(failingRemoteReader{err: want}, 4); !errors.Is(err, want) {
		t.Fatalf("reader error = %v, want %v", err, want)
	}
}

func TestStripePayoutOversizedResponseRemainsOutcomeUnknown(t *testing.T) {
	reader := io.LimitReader(zeroRemoteReader{}, stripeAPIResponseMaxBytes+1)
	body, err := readStripePayoutResponseBody(reader)
	if body != nil {
		t.Fatalf("oversized payout response returned %d bytes", len(body))
	}
	if !errors.Is(err, errPayoutOutcomeUnknown) {
		t.Fatalf("oversized payout response error = %v, want outcome unknown", err)
	}
}

func TestStripeHelpersRejectOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{name: "form post", call: func(ctx context.Context) error {
			_, err := stripeForm(ctx, "customers", url.Values{}, "")
			return err
		}},
		{name: "get", call: func(ctx context.Context) error {
			_, err := stripeGet(ctx, "accounts/acct_test")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStripeTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.CopyN(w, zeroRemoteReader{}, stripeAPIResponseMaxBytes+1)
			}))
			if err := tc.call(context.Background()); !errors.Is(err, errRemoteResponseTooLarge) {
				t.Fatalf("oversized Stripe response error = %v, want %v", err, errRemoteResponseTooLarge)
			}
		})
	}
}

type failingRemoteReader struct{ err error }

func (r failingRemoteReader) Read([]byte) (int, error) { return 0, r.err }

type zeroRemoteReader struct{}

func (zeroRemoteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

type remoteRoundTripFunc func(*http.Request) (*http.Response, error)

func (f remoteRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
