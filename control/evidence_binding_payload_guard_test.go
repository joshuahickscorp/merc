package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bindingMetaKeys are owned by the evidence-binding layer. They must never
// appear inside a job-result payload: those files are decoded with
// DisallowUnknownFields under their job contracts (see result_validation.go).
var bindingMetaKeys = []string{
	"binding_status",
	"missing_identity_fields",
	"producer_identity",
}

// TestContractGovernedEvidenceArtifactsAreNotStampedInPlace is the regression
// gate for the 1de03bf0 stamp bug: binding_status was written into every JSON
// object under evidence/, including worker-committed embed result artifacts
// that the control plane refuses when an unknown field is present.
//
// The rule: if a file, after stripping binding-layer keys, validates as a
// retained job-result contract, those binding keys must not have been in the
// file. Binding for payloads lives in a sibling `.binding.json` sidecar.
//
// This fails on the tree that stamped embed-candle_metal.json /
// embed-llama_cpp_metal.json in place, and passes once those bindings are
// sidecars.
func TestContractGovernedEvidenceArtifactsAreNotStampedInPlace(t *testing.T) {
	root := filepath.Join("..", "evidence")
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Skip("evidence/ not present")
	}

	var violations []string
	var scanned int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".binding.json") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
			// Non-object JSON is not a job-result envelope; sidecars cover it.
			return nil
		}
		scanned++

		present := bindingMetaPresent(raw)
		stripped := stripBindingMeta(raw)
		strippedBytes, err := json.Marshal(stripped)
		mustf(t, err, "re-marshal %s: %v", path)

		// Try each retained job contract. Model ref is taken from the payload
		// when present so known-model dimension checks still run.
		modelRef, _ := stripped["model"].(string)
		for _, jobType := range []string{"embed", "batch_infer"} {
			info := &CommitTaskInfo{jobType: jobType, ModelRef: modelRef}
			if err := validateTaskResultArtifact(info, strippedBytes); err != nil {
				continue
			}
			// This file is a contract-governed job-result payload.
			if len(present) > 0 {
				rel, _ := filepath.Rel(filepath.Join(".."), path)
				violations = append(violations,
					rel+" is a "+jobType+" job-result payload but carries in-object binding fields "+
						strings.Join(present, ", "))
			}
			// Sidecar is required so the binding rule still covers the file.
			side := path + ".binding.json"
			if _, err := os.Stat(side); err != nil {
				rel, _ := filepath.Rel(filepath.Join(".."), path)
				violations = append(violations,
					rel+" is a "+jobType+" job-result payload without "+filepath.Base(side))
			}
			break
		}
		return nil
	})
	must(t, err)
	if scanned == 0 {
		t.Fatal("walked evidence/ but found no JSON objects; the guard is vacuous")
	}
	if len(violations) > 0 {
		t.Fatalf("contract-governed evidence payloads stamped in place (%d):\n  - %s",
			len(violations), strings.Join(violations, "\n  - "))
	}
}

func bindingMetaPresent(raw map[string]any) []string {
	var out []string
	for _, k := range bindingMetaKeys {
		if _, ok := raw[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

func stripBindingMeta(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	skip := map[string]struct{}{}
	for _, k := range bindingMetaKeys {
		skip[k] = struct{}{}
	}
	for k, v := range raw {
		if _, drop := skip[k]; drop {
			continue
		}
		out[k] = v
	}
	return out
}
