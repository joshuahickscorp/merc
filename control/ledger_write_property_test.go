package main

import (
	"context"
	"math/rand"
	"testing"
	"testing/quick"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLedgerWriterMicroRoundTripProperty proves random int64 micro-USD amounts
// (including negatives and the NUMERIC(12,6) domain bounds) survive a round-trip
// through insertLedgerEntryTx exactly. No ad-hoc SQL is added for money writes.
func TestLedgerWriterMicroRoundTripProperty(t *testing.T) {
	ctx, _, pool := openPayoutTestStore(t)

	supplierID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO suppliers (id,email,reputation,status)
		VALUES ($1,$2,0.5,'active')`,
		supplierID, supplierID.String()+"@ledger-prop.invalid"); err != nil {
		t.Fatalf("seed supplier: %v", err)
	}

	boundCases := []int64{
		0,
		1,
		-1,
		maxMoneyAbsMicros,
		-maxMoneyAbsMicros,
		1_000_000,
		-1_000_000,
		123_456,
		-7,
		50_000_123_456, // $50_000.123456 — needs NUMERIC(12,6)
	}
	for _, micros := range boundCases {
		if err := validateLedgerInsert(ledgerInsert{Kind: KindPlatformTake, AmountMicros: micros}); err != nil {
			t.Fatalf("validate bound %d: %v", micros, err)
		}
		roundTripLedgerMicros(t, ctx, pool, supplierID, micros)
	}

	for _, bad := range []int64{maxMoneyAbsMicros + 1, -maxMoneyAbsMicros - 1, 1 << 50, -(1 << 50)} {
		if err := validateLedgerInsert(ledgerInsert{Kind: KindPlatformTake, AmountMicros: bad}); err == nil {
			t.Fatalf("expected domain rejection for %d", bad)
		}
	}

	cfg := &quick.Config{
		MaxCount: 64,
		Rand:     rand.New(rand.NewSource(42)),
	}
	err := quick.Check(func(raw int64) bool {
		var micros int64
		switch raw % 17 {
		case 0:
			micros = 0
		case 1:
			micros = maxMoneyAbsMicros
		case 2:
			micros = -maxMoneyAbsMicros
		default:
			mod := maxMoneyAbsMicros + 1
			if raw == -1<<63 {
				micros = 0
			} else if raw < 0 {
				micros = -((-raw) % mod)
			} else {
				micros = raw % mod
			}
		}
		if err := validateLedgerInsert(ledgerInsert{Kind: KindPlatformTake, AmountMicros: micros}); err != nil {
			t.Logf("validate %d: %v", micros, err)
			return false
		}
		roundTripLedgerMicros(t, ctx, pool, supplierID, micros)
		return true
	}, cfg)
	if err != nil {
		t.Fatalf("property: %v", err)
	}
}

func roundTripLedgerMicros(t *testing.T, ctx context.Context, pool *pgxpool.Pool, supplierID uuid.UUID, micros int64) {
	t.Helper()
	// Unique payout_ref tags this exact write so the read-back cannot collide
	// with another property sample that happens to share the same micros.
	ref := "prop-" + uuid.NewString()
	entry := ledgerInsert{
		Kind:         KindPlatformTake,
		AmountMicros: micros,
		PayoutStatus: PayoutReleased,
		PayoutRef:    ref,
	}
	if micros > 0 {
		sid := supplierID
		entry = ledgerInsert{
			Kind:         KindSupplierCredit,
			SupplierID:   &sid,
			AmountMicros: micros,
			PayoutStatus: PayoutHeld,
			PayoutRef:    ref,
		}
	}
	ct, err := insertLedgerEntryTx(ctx, pool, entry)
	if err != nil {
		t.Fatalf("insert micros=%d: %v", micros, err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("insert micros=%d rows=%d", micros, ct.RowsAffected())
	}
	var got int64
	if err := pool.QueryRow(ctx, `
		SELECT (amount_usd * 1000000)::bigint
		  FROM ledger_entries
		 WHERE payout_ref=$1`, ref).Scan(&got); err != nil {
		t.Fatalf("read back micros=%d: %v", micros, err)
	}
	if got != micros {
		t.Fatalf("round-trip micros want %d got %d (usd=%v)", micros, got, microsToUSD(micros))
	}
}
