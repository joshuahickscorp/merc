package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuyerModelKindDefaultsToServerRuntimeAuthority(t *testing.T) {
	body, err := json.Marshal(cliJobSubmit{
		JobType: jobType{Type: "embed"},
		Model:   modelRef{Ref: "all-minilm-l6-v2"},
		Input:   json.RawMessage(`"{\"text\":\"x\"}\n"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"kind"`) {
		t.Fatalf("default CLI request overrode generated server wire-kind authority: %s", body)
	}
	if !strings.Contains(string(body), `"model":{"ref":"all-minilm-l6-v2"}`) {
		t.Fatalf("model ref missing from CLI request: %s", body)
	}

	body, err = json.Marshal(cliJobSubmit{
		JobType: jobType{Type: "embed"},
		Model:   modelRef{Kind: "hf", Ref: "all-minilm-l6-v2"},
		Input:   json.RawMessage(`"{\"text\":\"x\"}\n"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"kind":"hf"`) {
		t.Fatalf("explicit compatibility kind did not survive encoding: %s", body)
	}
}
