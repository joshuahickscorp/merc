// Authoritative gateway parity harness (v2) — thin live driver.
//
// All measurement, statistics, identity proof, and gate evaluation live in
// src/control/gateway_parity_harness.go (and src/control/gateway_parity_cli.go).
// This file deliberately re-execs the control package so there is exactly one
// implementation. Do not re-embed client/pool/gate logic here.
//
//	go run ops/scripts/gateway-parity-v2.go \
//	  -merc-base-url http://127.0.0.1:8080/v1 \
//	  -direct-base-url http://127.0.0.1:8095/v1 \
//	  -model cx-chat-1b \
//	  -model-digest <64 hex> \
//	  -out evidence/perf/gateway-parity-v2.json
//
// Self-test (HARNESS_SELF_TEST, never comparable):
//
//	go run ops/scripts/gateway-parity-v2.go -self-test-standin -out /tmp/parity-selftest.json
//
// Equivalent direct invocation:
//
//	cd src/control && go run . gateway-parity -self-test-standin -out …
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	controlDir, err := locateControlDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	args := append([]string{"run", ".", "gateway-parity"}, os.Args[1:]...)
	cmd := exec.Command("go", args...)
	cmd.Dir = controlDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func locateControlDir() (string, error) {
	// Prefer sibling of this source file (works under go run).
	_, thisFile, _, ok := runtime.Caller(0)
	if ok {
		cand := filepath.Join(filepath.Dir(thisFile), "..", "..", "src", "control")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
	}
	// Fallback: cwd or cwd/control.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, cand := range []string{
		filepath.Join(cwd, "src", "control"),
		cwd,
		filepath.Join(cwd, "..", "src", "control"),
	} {
		if st, err := os.Stat(filepath.Join(cand, "gateway_parity_harness.go")); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("cannot locate src/control/ (run from repo root or ops/scripts/)")
}
