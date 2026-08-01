package main

import (
	"flag"
	"fmt"
	"os"
)

// dispatchProject exposes the compiler as a product command. It intentionally
// needs no database or server: static inspection happens on the buyer's machine,
// before any project artifact is uploaded or any paid compute is authorised.
func dispatchProject(command string, args []string) bool {
	if command != "project" {
		return false
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: merc project {compile|calibration-check}")
		os.Exit(2)
	}
	switch args[0] {
	case "compile":
		os.Exit(runProjectCompile(args[1:]))
	case "calibration-check":
		os.Exit(runProjectCalibrationCheck(args[1:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown project subcommand %q\n", args[0])
		os.Exit(2)
	}
	return true
}

func runProjectCompile(args []string) int {
	fs := flag.NewFlagSet("project compile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory to inspect")
	probe := fs.Bool("probe", false, "run non-executing bounded file-shape probes")
	approved := fs.String("buyer-approved-ir-sha256", "", "exact unprobed IR digest approved by the buyer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "project compile: --root is required")
		return 2
	}
	ir, err := compileProject(projectCompileOptions{Root: *root, ProbeRequested: *probe, BuyerApprovedIRSHA256: *approved})
	if err != nil {
		fmt.Fprintf(os.Stderr, "project compile: %v\n", err)
		return 1
	}
	if err := writeProjectIR(os.Stdout, ir); err != nil {
		fmt.Fprintf(os.Stderr, "project compile: %v\n", err)
		return 1
	}
	return 0
}

func runProjectCalibrationCheck(args []string) int {
	fs := flag.NewFlagSet("project calibration-check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	path := fs.String("cohort", "", "outcome-linked project calibration cohort JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "project calibration-check: --cohort is required")
		return 2
	}
	info, err := os.Stat(*path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > projectCalibrationMaxBytes {
		fmt.Fprintf(os.Stderr, "project calibration-check: cohort must be a regular file no larger than %d bytes\n", projectCalibrationMaxBytes)
		return 1
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project calibration-check: %v\n", err)
		return 1
	}
	cohort, err := decodeProjectCalibrationCohort(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project calibration-check: %v\n", err)
		return 1
	}
	result := EvaluateProjectCalibration(cohort)
	if err := writeProjectCalibrationResult(os.Stdout, result); err != nil {
		fmt.Fprintf(os.Stderr, "project calibration-check: %v\n", err)
		return 1
	}
	if !result.PromotableForEstimation {
		return 1
	}
	return 0
}
