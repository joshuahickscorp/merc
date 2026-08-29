package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The network-fault register is the half of Step 22 that says what is NOT
// covered, and a register nobody checks decays into prose within a release.
//
// Two ways it can lie, and this test closes both:
//
//   - a REGISTERED entry names a mutation id that no longer exists, so the
//     register claims coverage the manifest cannot supply;
//   - a DEFERRED entry claims an authority is absent when someone has since
//     built it, so a fault that could be attacked today stays parked forever
//     behind a stale excuse.
//
// The second is the dangerous one. An absent authority is a legitimate reason
// to defer exactly once; it becomes an alibi the moment it stops being true.
type networkFaultRegister struct {
	Registered []struct {
		Fault       string `json:"fault"`
		MutationIDs []int  `json:"mutation_ids"`
		Target      string `json:"target"`
	} `json:"registered"`
	DeferredAbsent []struct {
		Fault          string `json:"fault"`
		WaitsOn        string `json:"waits_on"`
		VerifiedAbsent string `json:"verified_absent"`
	} `json:"deferred_absent_authority"`
}

func TestNetworkFaultRegisterMatchesTheManifestAndTheSource(t *testing.T) {
	root, err := repoRootForTest()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "ops", "scripts", "network-fault-register.json"))
	if err != nil {
		t.Fatalf("read register: %v", err)
	}
	var reg networkFaultRegister
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("register is not JSON: %v", err)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(root, "ops", "scripts", "mutation-manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Mutations []struct {
			ID int `json:"id"`
		} `json:"mutations"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	known := map[int]bool{}
	for _, m := range manifest.Mutations {
		known[m.ID] = true
	}

	if len(reg.Registered) == 0 {
		t.Fatal("the register lists no registered faults, which would make every deferral vacuous")
	}
	for _, entry := range reg.Registered {
		if len(entry.MutationIDs) == 0 {
			t.Fatalf("registered fault %q names no mutation id, so nothing tests it", entry.Fault)
		}
		for _, id := range entry.MutationIDs {
			if !known[id] {
				t.Fatalf("registered fault %q cites mutation %d, which is not in the manifest: "+
					"the register claims coverage the catalogue cannot supply", entry.Fault, id)
			}
		}
	}

	// Every deferral must still be true. The verified_absent field names the
	// grep that established it; re-run that grep rather than trusting the note.
	patterns := map[string]string{
		"Make failed region healthy": `region_health|failed_region|region_status|RegionHealth`,
		"Coherent-epoch corruption, candidate-index poisoning, twin-only faults": `coherent_epoch|CoherentEpoch|candidate_index`,
	}
	if len(reg.DeferredAbsent) == 0 {
		t.Fatal("no deferred faults are listed; Step 22 completes only if the catalogue " +
			"explicitly lists the faults it cannot attack")
	}
	for _, entry := range reg.DeferredAbsent {
		pattern, checkable := patterns[entry.Fault]
		if !checkable {
			// Deferrals whose absence is not a grep (e.g. an undetermined
			// question) still must say what they wait on.
			if strings.TrimSpace(entry.WaitsOn) == "" {
				t.Fatalf("deferred fault %q names no authority it waits on", entry.Fault)
			}
			continue
		}
		cmd := exec.Command("grep", "-rlE", pattern, filepath.Join(root, "src", "control"),
			"--include=*.go")
		out, _ := cmd.Output()
		var live []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" || strings.HasSuffix(line, "_test.go") {
				continue
			}
			live = append(live, line)
		}
		if len(live) > 0 {
			t.Fatalf("deferred fault %q claims its authority is absent, but %v now match %q — "+
				"the deferral has become an alibi and the fault is registerable today",
				entry.Fault, live, pattern)
		}
	}
}

// repoRootForTest resolves the repository root from the src/control/ working dir.
func repoRootForTest() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
