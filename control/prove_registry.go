package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func cmdProve(args []string) {
	mode, artifactDir := "full", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--contract-only":
			mode = "contract"
		case "--full":
			mode = "full"
		case "--artifact-dir":
			artifactDir = next(args, &i)
		default:
			fatalf("cx prove: unknown flag %q", args[i])
		}
	}
	root := repoRootOrCwd()
	if artifactDir == "" {
		artifactDir = filepath.Join(root, ".artifacts", "prove-local")
	} else if abs, err := filepath.Abs(artifactDir); err == nil {
		artifactDir = abs
	}
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "prove-local.sh"))
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CX_PROOF_ARTIFACT_DIR="+artifactDir)
	if mode == "contract" {
		cmd.Env = append(cmd.Env, "SKIP_LIVE=1")
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cx prove: %s proof failed: %v\n", mode, err)
		os.Exit(1)
	}
	ledger := filepath.Join(artifactDir, "ledger.jsonl")
	result, err := verifyProofLedger(ledger, mode, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cx prove: invalid proof ledger: %v\n", err)
		os.Exit(1)
	}
	b, _ := canonicalProofJSON(map[string]any{"status": "PASS", "proof": result})
	fmt.Println(string(b))
}
