package main

import "testing"

// must fails the test if err is non-nil. t.Helper keeps the failure line at the
// call site so diagnostics still name what failed and where.
func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mustf fails with a formatted message when err is non-nil. The format must
// accept err as its final argument (typically via a trailing %v), matching the
// prior t.Fatalf("…: %v", …, err) shape so message quality is preserved.
func mustf(t testing.TB, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(format, append(args, err)...)
	}
}
