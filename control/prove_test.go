package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSrc = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSta = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func terminalLedger() []string {
	return []string{
		"META\tcommit\t" + strings.Repeat("c", 40),
		"META\tdirty\ttrue",
		"META\tsource_sha256\t" + testSrc,
		"META\tstatus_sha256\t" + testSta,
		"META\tstarted_at\t2026-07-10T00:00:00Z",
		"META\tproof_mode\tcontract_only",
		"PASS\tmatrix:TestAtomicThing\tdeterministic check",
		"META\tsource_sha256_end\t" + testSrc,
		"META\tstatus_sha256_end\t" + testSta,
		"PASS\tsource-stability\tstable",
		"META\tcompleted_at\t2026-07-10T00:01:00Z",
		"META\tstatus\tPASS",
	}
}

func writeLedger(t *testing.T, rows []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.txt")
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyAcceptsTerminalSourceBound(t *testing.T) {
	p := writeLedger(t, terminalLedger())
	res, err := validateLedger(p, "contract_only", []string{"matrix:TestAtomicThing"}, false, "")
	if err != nil {
		t.Fatalf("expected PASS, got %v", err)
	}
	if res.meta["status"] != "PASS" {
		t.Errorf("status = %q", res.meta["status"])
	}
}

func TestVerifyRejects(t *testing.T) {
	swap := func(rows []string, from, to string) []string {
		out := append([]string(nil), rows...)
		for i, r := range out {
			if r == from {
				out[i] = to
			}
		}
		return out
	}
	drop := func(rows []string, prefix string) []string {
		var out []string
		for _, r := range rows {
			if !strings.HasPrefix(r, prefix) {
				out = append(out, r)
			}
		}
		return out
	}
	cases := []struct {
		name    string
		rows    []string
		mode    string
		wantMsg string
	}{
		{"status-not-pass", swap(terminalLedger(), "META\tstatus\tPASS", "META\tstatus\tFAIL"), "", "terminal status is 'FAIL', not PASS"},
		{"fail-row", append(terminalLedger(), "FAIL\tmatrix:Boom\tbroke"), "", "ledger contains FAIL rows: matrix:Boom"},
		{"src-differ", swap(terminalLedger(), "META\tsource_sha256_end\t"+testSrc, "META\tsource_sha256_end\t"+strings.Repeat("d", 64)), "", "start/end source fingerprints differ"},
		{"missing-stability", drop(terminalLedger(), "PASS\tsource-stability"), "", "source-stability PASS is missing"},
		{"malformed-row", []string{"META\tonly-two"}, "", "line 1 is not a three-column ledger row"},
		{"mode-mismatch", terminalLedger(), "full_local", "proof mode 'contract_only' cannot satisfy required mode 'full_local'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeLedger(t, c.rows)
			_, err := validateLedger(p, c.mode, nil, false, "")
			if err == nil {
				t.Fatalf("expected rejection %q, got PASS", c.wantMsg)
			}
			if err.Error() != c.wantMsg {
				t.Errorf("message = %q, want %q", err.Error(), c.wantMsg)
			}
		})
	}
}

func TestVerifyDuplicateMetaRejected(t *testing.T) {
	rows := append(terminalLedger(), "META\tcommit\tzzz")
	p := writeLedger(t, rows)
	_, err := validateLedger(p, "", nil, false, "")
	if err == nil || !strings.HasPrefix(err.Error(), "duplicate META key: commit") {
		t.Errorf("expected duplicate META rejection, got %v", err)
	}
}
