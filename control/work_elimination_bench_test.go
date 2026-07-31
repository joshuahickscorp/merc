package main

import (
	"encoding/json"
	"testing"
)

// What the tool/schema half of a realtime cache key actually costs.
//
// The programme asks for a tokenization cache and a tool/schema cache as
// work-elimination levers. This benchmark is why neither was built in the control
// plane, and it is kept so the decision can be re-tested rather than remembered.
//
// Measured on an M3 Ultra over a three-function tool set plus a strict
// json_schema response format — a large payload by the standards of real traffic:
//
//	BenchmarkCanonicalToolsAndSchema-28   10817 ns/op   7051 B/op   193 allocs/op
//
// 10.8µs against a measured 3.9ms of Merc realtime overhead and a 58ms median
// request: a perfect cache eliminates 0.3% of Merc's own cost and 0.02% of the
// buyer's latency. There is no buyer discount in that, and an entry in a
// work-elimination table claiming one would be decoration.
//
// The tokenization case is the same finding for a different reason: this control
// plane never tokenizes. Input depth is accumulated by inputDepthAccumulator while
// the input is ALREADY being streamed for upload in streamSplitAndUpload, so the
// work is fused with a read that has to happen anyway and there is nothing to
// avoid. Real tokenizer cost lives in the agent's runtime, on the other side of
// the wire, where a cache would be an agent change and not a Merc one.
//
// What DOES pay is already wired: in-flight coalescing (inflight_coalescing.go),
// exact-result reuse (exact_reuse.go) and prefix/KV-aware routing
// (prefix_routing.go). Those eliminate supplier EXECUTION, which is the term the
// buyer's price is made of.
func BenchmarkCanonicalToolsAndSchema(b *testing.B) {
	raw := []byte(`{"tools":[
      {"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["c","f"]},"days":{"type":"integer","minimum":1,"maximum":14}},"required":["city"]}}},
      {"type":"function","function":{"name":"search_docs","description":"Search the documentation corpus","parameters":{"type":"object","properties":{"query":{"type":"string"},"top_k":{"type":"integer"},"filters":{"type":"object","properties":{"product":{"type":"string"},"version":{"type":"string"},"language":{"type":"string"}}}},"required":["query"]}}},
      {"type":"function","function":{"name":"create_ticket","description":"Open a support ticket","parameters":{"type":"object","properties":{"title":{"type":"string"},"body":{"type":"string"},"severity":{"type":"string","enum":["p0","p1","p2","p3"]},"labels":{"type":"array","items":{"type":"string"}}},"required":["title","body"]}}}],
      "response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"},"citations":{"type":"array","items":{"type":"object","properties":{"url":{"type":"string"},"quote":{"type":"string"}}}}},"required":["answer"]}}}}`)
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := canonicalJSON(payload["tools"]); err != nil {
			b.Fatal(err)
		}
		if _, err := canonicalJSON(payload["response_format"]); err != nil {
			b.Fatal(err)
		}
	}
}
