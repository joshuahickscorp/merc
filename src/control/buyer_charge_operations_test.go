package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseCanonicalBuyerChargeOperationKey(t *testing.T) {
	id := uuid.New()
	for _, test := range []struct {
		name string
		key  string
		kind string
		ok   bool
	}{
		{name: "job", key: "job-" + id.String(), kind: "job", ok: true},
		{name: "batch", key: "cxbatch-" + id.String(), kind: "batch", ok: true},
		{name: "trimmed canonical key", key: " job-" + id.String() + " ", kind: "job", ok: true},
		{name: "operator suffix", key: "job-cas-" + id.String(), ok: false},
		{name: "non uuid", key: "job-not-a-uuid", ok: false},
		{name: "nil uuid", key: "job-" + uuid.Nil.String(), ok: false},
		{name: "unknown namespace", key: "topup-" + id.String(), ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, gotID, ok := parseCanonicalBuyerChargeOperationKey(test.key)
			if ok != test.ok || kind != test.kind || (ok && gotID != id) {
				t.Fatalf("parseCanonicalBuyerChargeOperationKey(%q)=(%q,%s,%v), want (%q,%s,%v)",
					test.key, kind, gotID, ok, test.kind, id, test.ok)
			}
		})
	}
}
