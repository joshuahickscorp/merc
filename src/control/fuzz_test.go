package main

import (
	"encoding/json"
	"testing"
)

func FuzzSplitJSONL(f *testing.F) {
	for _, s := range []string{"", "\n", "a\nb\nc", "{}\n{}\n", "\r\n\r\n", "x"} {
		f.Add([]byte(s), 2)
	}
	f.Fuzz(func(t *testing.T, data []byte, n int) {
		chunks := splitJSONL(data, n) // must not panic for any n (incl. <=0, huge)
		var got int
		for _, c := range chunks {
			if len(c) == 0 {
				t.Fatal("splitJSONL emitted an empty chunk")
			}
			for _, ln := range splitLinesForTest(c) {
				if len(ln) == 0 {
					t.Fatal("chunk contains a blank line (should have been dropped)")
				}
				got++
			}
		}
		if len(chunks) > got+1 {
			t.Fatalf("more chunks (%d) than lines (%d)", len(chunks), got)
		}
	})
}

func FuzzJobSubmitDecode(f *testing.F) {
	for _, s := range []string{
		`{}`,
		`{"job_type":{"type":"embed"},"input":"{}"}`,
		`{"job_type":{"type":"batch_infer","max_tokens":9999999999},"params":{"split_size":-1}}`,
		`{"verification":{"redundancy_frac":1e30}}`,
		`not json`,
		``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var sub jobSubmit
		if err := json.Unmarshal(body, &sub); err != nil {
			return // a decode error is the clean, expected outcome  -  never a panic
		}
		_ = splitSizeOf(sub.Params)
		_ = fracCount(1<<20, sub.Verification.RedundancyFrac)
		_ = fracCount(1<<20, sub.Verification.HoneypotFrac)
		if len(sub.Input) > 0 && sub.Input[0] == '"' {
			var inline string
			if json.Unmarshal(sub.Input, &inline) == nil {
				_ = splitJSONL([]byte(inline), splitSizeOf(sub.Params))
			}
		}
	})
}

func splitLinesForTest(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			ln := b[start:i]
			if len(ln) > 0 && ln[len(ln)-1] == '\r' {
				ln = ln[:len(ln)-1]
			}
			if len(ln) > 0 {
				out = append(out, ln)
			}
			start = i + 1
		}
	}
	return out
}
