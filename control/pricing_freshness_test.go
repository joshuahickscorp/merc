package main

import (
	"testing"
	"time"
)

// A vendor-owned observation scores confidence 1.0 and keeps it forever, so
// without an age rule a board fetched once would price the catalogue
// indefinitely. These tests pin the three properties that stops.

func freshBoard(boardDate string, observationDates ...string) *priceBoard {
	observations := make([]priceBoardObservation, 0, len(observationDates))
	for index, date := range observationDates {
		observations = append(observations, priceBoardObservation{
			Provider:  "openai",
			Model:     "m",
			USDPer1K:  0.01 * float64(index+1),
			SourceURL: "https://openai.com/pricing",
			FetchedAt: date,
		})
	}
	return &priceBoard{
		SchemaVersion:         1,
		Unit:                  "usd_per_1k_units",
		FetchedAt:             boardDate,
		PositioningMultiplier: 0.9,
		Classes: map[string]priceBoardClass{
			"embed_small": {JobType: "embed", Observations: observations},
		},
	}
}

func TestStaleObservationsAreNotEvidence(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	board := freshBoard(
		"2026-07-26",
		"2026-07-26", // fresh
		"2026-07-20", // fresh
		"2026-01-01", // far outside the ceiling
	)

	if err := dropStaleObservations(board, time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC), now); err != nil {
		t.Fatalf("fresh board rejected: %v", err)
	}
	kept := board.Classes["embed_small"].Observations
	if len(kept) != 2 {
		t.Fatalf("expected the stale row dropped, kept %d rows", len(kept))
	}

	// Dropping it takes the class under the evidence floor, so the class is
	// refused rather than priced from two rows. The previous catalogue price
	// stands - that is the intended failure mode.
	if _, _, err := confidenceWeightedMedianUSDPer1K("embed_small", board.Classes["embed_small"]); err == nil {
		t.Fatal("a class left under the observation floor must be refused, not priced")
	}
}

func TestAWhollyStaleBoardIsRefused(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	board := freshBoard("2026-01-01", "2026-01-01")
	boardAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	err := dropStaleObservations(board, boardAt, now)
	if err == nil {
		t.Fatal("a board past the age ceiling must be refused outright")
	}
	// Refusing must name the age, so an operator can see why pricing stopped.
	if got := err.Error(); got == "" || !contains(got, "days old") {
		t.Fatalf("refusal does not report the board age: %q", got)
	}
}

func TestABoardDatedInTheFutureIsRefused(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	board := freshBoard("2026-09-01", "2026-09-01")
	boardAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	if err := dropStaleObservations(board, boardAt, now); err == nil {
		t.Fatal("a board dated in the future must be refused")
	}
}

func TestAnUndatedObservationInheritsTheBoardDate(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	boardAt := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	board := freshBoard("2026-07-26", "", "", "")

	if err := dropStaleObservations(board, boardAt, now); err != nil {
		t.Fatalf("fresh undated rows rejected: %v", err)
	}
	if got := len(board.Classes["embed_small"].Observations); got != 3 {
		t.Fatalf("undated rows should inherit a fresh board date, kept %d", got)
	}

	// The same rows against an old board are stale, so an undated row cannot
	// outlive the board it came from.
	oldAt := now.Add(-(maxObservationAge + 24*time.Hour))
	old := freshBoard(oldAt.Format("2006-01-02"), "", "", "")
	if err := dropStaleObservations(old, oldAt, now); err != nil {
		t.Fatalf("board inside the board ceiling rejected: %v", err)
	}
	if got := len(old.Classes["embed_small"].Observations); got != 0 {
		t.Fatalf("undated rows on a stale board should be dropped, kept %d", got)
	}
}

func TestAnUnparseableObservationDateIsNotFresh(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	boardAt := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	board := freshBoard("2026-07-26", "not-a-date", "2026-07-26")

	if err := dropStaleObservations(board, boardAt, now); err != nil {
		t.Fatalf("board rejected: %v", err)
	}
	if got := len(board.Classes["embed_small"].Observations); got != 1 {
		t.Fatalf("an unparseable date must not count as fresh, kept %d", got)
	}
}

func TestBoardTimestampAcceptsBothPublishedForms(t *testing.T) {
	for _, value := range []string{"2026-07-26", "2026-07-26T12:00:00Z", "2026-07-26T12:00:00+00:00"} {
		if _, err := parseBoardTimestamp(value); err != nil {
			t.Fatalf("published timestamp form %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "   ", "26-07-2026", "yesterday"} {
		if _, err := parseBoardTimestamp(value); err == nil {
			t.Fatalf("unusable timestamp %q accepted", value)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// pinBoardClockForPublication freezes publication's clock a day after the
// committed board's own fetch date. Without it, every test that publishes from
// the committed board becomes a dated failure: the board ages past the ceiling
// on a calendar date, with no code change, and CI breaks for no reason a
// reviewer could see in a diff.
func pinBoardClockForPublication(t *testing.T) {
	t.Helper()
	board, err := loadPriceBoard()
	if err != nil {
		t.Fatalf("load price board: %v", err)
	}
	at, err := parseBoardTimestamp(board.FetchedAt)
	if err != nil {
		t.Fatalf("committed board has an unusable fetched_at: %v", err)
	}
	previous := priceBoardNow
	priceBoardNow = func() time.Time { return at.Add(24 * time.Hour) }
	t.Cleanup(func() { priceBoardNow = previous })
}

func TestPublicationRefusesAStaleBoard(t *testing.T) {
	board, err := loadPriceBoard()
	if err != nil {
		t.Fatalf("load price board: %v", err)
	}
	at, err := parseBoardTimestamp(board.FetchedAt)
	if err != nil {
		t.Fatalf("parse board date: %v", err)
	}

	// Just inside the ceiling the board still publishes.
	if _, err := boardAsOfPublication(board, at.Add(maxBoardAge-time.Hour)); err != nil {
		t.Fatalf("board inside the age ceiling was refused: %v", err)
	}
	// Past it, publication refuses rather than minting authority from stale evidence.
	if _, err := boardAsOfPublication(board, at.Add(maxBoardAge+time.Hour)); err == nil {
		t.Fatal("publication accepted a board past the age ceiling")
	}
}

func TestPublicationDoesNotMutateTheCachedBoard(t *testing.T) {
	board, err := loadPriceBoard()
	if err != nil {
		t.Fatalf("load price board: %v", err)
	}
	at, err := parseBoardTimestamp(board.FetchedAt)
	if err != nil {
		t.Fatalf("parse board date: %v", err)
	}
	before := map[string]int{}
	for name, class := range board.Classes {
		before[name] = len(class.Observations)
	}

	// Age every row out. The read-only price page renders the cached board, so
	// publication must not strip rows from underneath it.
	if _, err := boardAsOfPublication(board, at.Add(maxObservationAge+time.Hour)); err != nil {
		// A refusal is fine here; the point is what it did to the cache.
		_ = err
	}
	for name, class := range board.Classes {
		if len(class.Observations) != before[name] {
			t.Fatalf("publication mutated the cached board: class %q went from %d to %d rows",
				name, before[name], len(class.Observations))
		}
	}
}
