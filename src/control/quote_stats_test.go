package main

import "testing"

func TestScanJSONLUsedAndSkipped(t *testing.T) {
	data := []byte("{\"text\":\"a\"}\n\n{\"text\":\"b\"}\nnot json")
	scan := scanJSONL(data)
	if scan.Records != 3 {
		t.Fatalf("records (non-blank) = %d, want 3", scan.Records)
	}
	if scan.MalformedRecords != 1 {
		t.Fatalf("malformed = %d, want 1", scan.MalformedRecords)
	}
	if scan.BlankRecords != 1 {
		t.Fatalf("blank = %d, want 1", scan.BlankRecords)
	}
	if scan.SkippedRecords != 2 {
		t.Fatalf("skipped = %d, want 2 (1 blank + 1 malformed)", scan.SkippedRecords)
	}
	if scan.Bytes == 0 {
		t.Fatal("bytes must be counted")
	}
}
