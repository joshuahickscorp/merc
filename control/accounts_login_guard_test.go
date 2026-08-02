package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoginGuardStaysBoundedUnderSustainedLockedFailures(t *testing.T) {
	const cap = 32
	g := newLoginGuard(cap)
	now := time.Now()

	// Drive every entry into a still-active lockout, then keep flooding with
	// distinct attacker-chosen emails. The map must never exceed the cap.
	for i := 0; i < cap*4; i++ {
		email := strings.ToLower(strings.TrimSpace(
			"attacker-" + strings.Repeat("x", 8) + "-" + itoa(i) + "@evil.example",
		))
		// Accumulate fails until locked so every resident entry is locked.
		for f := 0; f < maxLoginFails; f++ {
			g.fail(email, now)
		}
		if ok, _ := g.allow(email, now); ok && i < cap {
			// First wave should lock; if allow still returns true the fail
			// counter did not engage — surface that before the bound check.
			t.Fatalf("entry %d should be locked after %d fails", i, maxLoginFails)
		}
		if n := g.len(); n > cap {
			t.Fatalf("loginGuard grew past cap: len=%d cap=%d after %d distinct locked emails", n, cap, i+1)
		}
	}
	if n := g.len(); n > cap {
		t.Fatalf("final size %d exceeds cap %d", n, cap)
	}
	if n := g.len(); n == 0 {
		t.Fatal("expected some locked entries to remain under cap")
	}
}

func TestLoginGuardRejectsOverlongAndMalformedIdentifiers(t *testing.T) {
	cases := []struct {
		name  string
		email string
	}{
		{"empty", ""},
		{"no-at", "not-an-email"},
		{"no-domain-dot", "user@localhost"},
		{"leading-dot-domain", "user@.example.com"},
		{"trailing-dot-domain", "user@example.com."},
		{"overlong", strings.Repeat("a", 250) + "@example.com"}, // 262 > 254
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email := normalizeEmail(tc.email)
			if validLoginIdentifier(email) {
				t.Fatalf("expected reject for %q (len=%d)", email, len(email))
			}
		})
	}

	// Legitimate shape and length must still be accepted — no behaviour change
	// for a normal login identifier.
	if !validLoginIdentifier(normalizeEmail("buyer@example.com")) {
		t.Fatal("well-formed email must remain admissible")
	}
	// Exactly at the RFC bound.
	local := strings.Repeat("a", maxLoginEmailLen-len("@example.com"))
	atBound := local + "@example.com"
	if len(atBound) != maxLoginEmailLen {
		t.Fatalf("test setup: want len %d, got %d", maxLoginEmailLen, len(atBound))
	}
	if !validLoginIdentifier(atBound) {
		t.Fatal("email of exactly 254 characters must be admissible")
	}
}

func TestLoginGuardOldestFirstEvictsWhileLocked(t *testing.T) {
	g := newLoginGuard(2)
	now := time.Now()
	// Three distinct locked identities; third insert must evict the oldest.
	for i, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		touch := now.Add(time.Duration(i) * time.Second)
		for f := 0; f < maxLoginFails; f++ {
			g.fail(email, touch)
		}
	}
	if n := g.len(); n != 2 {
		t.Fatalf("want cap=2 residents, got %d", n)
	}
	// Oldest (a@) should be gone; b and c remain.
	if ok, _ := g.allow("a@example.com", now.Add(10*time.Second)); !ok {
		// If a is still present and locked, allow returns false.
		t.Fatal("oldest locked entry should have been evicted to honour the cap")
	}
	if ok, _ := g.allow("c@example.com", now.Add(10*time.Second)); ok {
		t.Fatal("newest locked entry must still be present and locking")
	}
}

func TestLoginGuardSweepDropsIdleUnlockedEntries(t *testing.T) {
	g := newLoginGuard(8)
	now := time.Now()
	g.fail("idle@example.com", now)
	// Not yet locked (fails < maxLoginFails), touched at now.
	g.sweep(now.Add(loginLockoutMax + time.Second))
	if g.len() != 0 {
		t.Fatalf("sweep should drop idle unlocked entries, len=%d", g.len())
	}
}

// tiny decimal helper so this file stays free of strconv for a pure unit test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
