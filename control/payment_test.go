package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSplitFrozenChargeDoesNotLeakBuyerSafetyFeeToSupplier(t *testing.T) {
	buyer, supplier, task := uuid.New(), uuid.New(), uuid.New()
	entries := splitFrozenCharge(buyer, supplier, task, 0.50, 0.05, 90, time.Unix(100, 0))
	if len(entries) != 3 {
		t.Fatalf("entries=%d want 3", len(entries))
	}
	if entries[0].Kind != KindBuyerCharge || entries[0].AmountUSD != -0.50 {
		t.Fatalf("buyer entry=%+v", entries[0])
	}
	if entries[1].Kind != KindSupplierCredit || entries[1].AmountUSD != 0.05 {
		t.Fatalf("supplier entry=%+v; payout must be the frozen amount, not a percentage of buyer charge", entries[1])
	}
	if entries[2].Kind != KindPlatformTake || entries[2].AmountUSD != 0.45 {
		t.Fatalf("platform entry=%+v", entries[2])
	}
	if entries[1].ReleaseAt == nil || !entries[1].ReleaseAt.Equal(time.Unix(100, 0).Add(minimumPayoutHold)) {
		t.Fatalf("supplier hold=%v", entries[1].ReleaseAt)
	}
}

func TestPayoutReleaseAtEnforcesServerFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, requested := range []uint32{0, 1, uint32((minimumPayoutHold - time.Second) / time.Second)} {
		if got, want := payoutReleaseAt(now, requested), now.Add(minimumPayoutHold); !got.Equal(want) {
			t.Fatalf("payoutReleaseAt(%d) = %v, want server floor %v", requested, got, want)
		}
	}
	requested := uint32((minimumPayoutHold + 6*time.Hour) / time.Second)
	if got, want := payoutReleaseAt(now, requested), now.Add(minimumPayoutHold+6*time.Hour); !got.Equal(want) {
		t.Fatalf("payoutReleaseAt(long hold) = %v, want %v", got, want)
	}
}

func TestMarkPayoutRefusesReleasedWithoutRef(t *testing.T) {
	s := &Store{}
	if err := s.MarkPayout(context.Background(), uuid.New(), PayoutReleased, ""); err == nil {
		t.Fatal("MarkPayout(released, \"\") must error  -  releasing without a payout ref would fake a transfer")
	}
}

func TestManualExportPayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payouts.csv")
	p := newManualExportPayout(path)
	s1 := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	s2 := uuid.MustParse("00000000-0000-0000-0000-0000000000a2")

	key1 := uuid.NewString()
	result, err := p.Send(context.Background(), s1, 125, "usd", key1)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Ref != "manual-export:"+path || result.CashMoved {
		t.Fatalf("result = %+v, want non-cash manual-export:%s", result, path)
	}
	key2 := uuid.NewString()
	if _, err := p.Send(context.Background(), s2, 250, "usd", key2); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 exported rows, got %d: %q", len(lines), string(b))
	}
	if !strings.HasPrefix(lines[0], s1.String()+",1.250000,") {
		t.Fatalf("row 0 = %q, want %s,1.250000,<ts>", lines[0], s1)
	}
	if !strings.HasPrefix(lines[1], s2.String()+",2.500000,") {
		t.Fatalf("row 1 = %q", lines[1])
	}
	if _, err := p.Send(context.Background(), s1, 125, "usd", key1); err != nil {
		t.Fatalf("idempotent export retry: %v", err)
	}
	if b2, _ := os.ReadFile(path); len(strings.Split(strings.TrimSpace(string(b2)), "\n")) != 2 {
		t.Fatal("manual export retry appended a duplicate payout instruction")
	}
	if _, err := p.Send(context.Background(), s2, 125, "usd", key1); err == nil {
		t.Fatal("manual export accepted a payout key rebound to another supplier")
	}

	if _, err := p.Send(context.Background(), uuid.New(), 0, "usd", uuid.NewString()); err == nil {
		t.Fatal("expected error for non-positive amount")
	}
	if b2, _ := os.ReadFile(path); len(strings.Split(strings.TrimSpace(string(b2)), "\n")) != 2 {
		t.Fatal("rejected payout must not append a row")
	}
}

func TestStripeIdempotencyKey(t *testing.T) {
	supplier := uuid.MustParse("00000000-0000-0000-0000-0000000000b1")
	const cents = int64(500) // identical amount in both cycles  -  the collision trigger

	entry1 := uuid.NewString()
	entry2 := uuid.NewString()
	k1 := stripeIdempotencyKey(supplier, cents, entry1)
	k2 := stripeIdempotencyKey(supplier, cents, entry2)
	if k1 == k2 {
		t.Fatalf("distinct payouts collided: both keyed %q  -  the second transfer would be silently dropped", k1)
	}

	if got := stripeIdempotencyKey(supplier, cents, entry1); got != k1 {
		t.Fatalf("retry of same payout changed key: %q != %q (would double-pay)", got, k1)
	}

	other := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	if stripeIdempotencyKey(other, cents, entry1) == k1 {
		t.Fatal("key not bound to supplier: same key for two suppliers")
	}
	if stripeIdempotencyKey(supplier, cents+1, entry1) == k1 {
		t.Fatal("key not bound to cents: same key for two amounts")
	}

	if got, want := stripeIdempotencyKey(supplier, cents, ""), "cx-"+supplier.String()+"-500"; got != want {
		t.Fatalf("empty-key fallback = %q, want %q", got, want)
	}
}

func TestSplitSupplierLiabilityMicrosFloorsCashAndCarriesExactly(t *testing.T) {
	tests := []struct {
		name      string
		liability int64
		cents     int64
		remainder int64
	}{
		{"below half cent", 4_999, 0, 4_999},
		{"exact half cent", 5_000, 0, 5_000},
		{"exact cent", 10_000, 1, 0},
		{"cent plus maximum carry", 19_999, 1, 9_999},
		{"many cents", 1_239_999, 123, 9_999},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cents, remainder, err := splitSupplierLiabilityMicros(tc.liability)
			if err != nil {
				t.Fatal(err)
			}
			if cents != tc.cents || remainder != tc.remainder {
				t.Fatalf("split(%d)=(%d,%d), want (%d,%d)",
					tc.liability, cents, remainder, tc.cents, tc.remainder)
			}
			if got := cents*microUSDPerCent + remainder; got != tc.liability {
				t.Fatalf("cash+carry=%d, want exact liability %d", got, tc.liability)
			}
		})
	}
	if cents, remainder, err := splitSupplierLiabilityMicros(0); err != nil || cents != 0 || remainder != 0 {
		t.Fatalf("zero liability split=(%d,%d,%v), want carried no-op", cents, remainder, err)
	}
	for _, invalid := range []int64{-1} {
		if _, _, err := splitSupplierLiabilityMicros(invalid); err == nil {
			t.Fatalf("split(%d) accepted a non-positive liability", invalid)
		}
	}
}
