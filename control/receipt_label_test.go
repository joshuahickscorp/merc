package main

import "testing"

func TestDeriveVerificationLabel(t *testing.T) {
	cases := []struct {
		name string
		v    Verification
		want string
	}{
		{"independent redundancy", Verification{RedundancyMatched: 1, Checked: 1}, "verified"},
		{"tiebreak", Verification{Tiebreaks: 1, Checked: 1}, "verified"},
		{"all delivered chunks verified", Verification{DeliveredChunks: 2, VerifiedChunks: 2, UnverifiedChunks: 0, RedundancyMatched: 2}, "fully-verified"},
		{"one of ten chunks verified", Verification{DeliveredChunks: 10, VerifiedChunks: 1, UnverifiedChunks: 9, RedundancyMatched: 1}, "sampled-verified"},
		{"delivered but unchecked", Verification{DeliveredChunks: 3, UnverifiedChunks: 3}, "unverified"},
		{"honeypot only", Verification{HoneypotsPassed: 2, Checked: 2}, "honeypot-checked"},
		{"same-supplier only", Verification{SameSupplier: 1}, "no-independent-peer"},
		{"cross-class only", Verification{CrossClassSkipped: 1}, "cross-class-skip"},
		{"nothing", Verification{}, "unverified"},
		{"legacy event-only verified beats gaps", Verification{RedundancyMatched: 1, SameSupplier: 1, CrossClassSkipped: 1, Checked: 1}, "verified"},
	}
	for _, c := range cases {
		if got := deriveVerificationLabel(c.v); got != c.want {
			t.Errorf("%s: label = %q, want %q", c.name, got, c.want)
		}
	}
}
