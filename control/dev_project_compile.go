package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type projectArtifactPaths []string

func (p *projectArtifactPaths) String() string { return strings.Join(*p, ",") }
func (p *projectArtifactPaths) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("artifact path must not be empty")
	}
	*p = append(*p, value)
	return nil
}

func runProjectSubmit(args []string) int {
	fs := flag.NewFlagSet("project submit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory to compile and submit")
	approved := fs.String("buyer-approved-ir-sha256", "", "exact unprobed IR digest approved by the buyer")
	quotePath := fs.String("quote", "", "reviewed project quote artifact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *quotePath == "" || !validSHA256(*approved) {
		fmt.Fprintln(os.Stderr, "project submit: --root, --quote and --buyer-approved-ir-sha256 are required")
		return 2
	}
	info, err := os.Lstat(*quotePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > projectQuoteArtifactMaxBytes {
		fmt.Fprintf(os.Stderr, "project submit: quote must be a regular non-symlink file no larger than %d bytes\n", projectQuoteArtifactMaxBytes)
		return 1
	}
	raw, err := os.ReadFile(*quotePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project submit: %v\n", err)
		return 1
	}
	var quote ProjectQuote
	if err := decodeStrictJSONObject(raw, &quote); err != nil {
		fmt.Fprintf(os.Stderr, "project submit: invalid quote artifact: %v\n", err)
		return 1
	}
	ir, err := compileProject(projectCompileOptions{Root: *root, ProbeRequested: true, BuyerApprovedIRSHA256: *approved})
	if err != nil {
		fmt.Fprintf(os.Stderr, "project submit: %v\n", err)
		return 1
	}
	result, err := submitCompiledProject(newClient(), *root, ir, quote, time.Now())
	if writeErr := writeProjectSubmission(os.Stdout, result); writeErr != nil {
		fmt.Fprintf(os.Stderr, "project submit: %v\n", writeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "project submit: %v\n", err)
		return 1
	}
	return 0
}

func writeProjectSubmission(w io.Writer, submission ProjectSubmission) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(submission)
}

// dispatchProject exposes the compiler as a product command. It intentionally
// needs no database or server: static inspection happens on the buyer's machine,
// before any project artifact is uploaded or any paid compute is authorised.
func dispatchProject(command string, args []string) bool {
	if command != "project" {
		return false
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: merc project {contracts|compile|quote|submit|quote-roots|submit-roots|materialize|quote-step|submit-step|calibration-check}")
		os.Exit(2)
	}
	switch args[0] {
	case "compile":
		os.Exit(runProjectCompile(args[1:]))
	case "calibration-check":
		os.Exit(runProjectCalibrationCheck(args[1:]))
	case "contracts":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: merc project contracts")
			os.Exit(2)
		}
		contracts, err := advertisedProjectRuntimeContracts()
		if err != nil {
			fmt.Fprintf(os.Stderr, "project contracts: %v\n", err)
			os.Exit(1)
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(contracts); err != nil {
			fmt.Fprintf(os.Stderr, "project contracts: %v\n", err)
			os.Exit(1)
		}
		return true
	case "quote":
		os.Exit(runProjectQuote(args[1:]))
	case "submit":
		os.Exit(runProjectSubmit(args[1:]))
	case "quote-roots":
		os.Exit(runProjectQuoteRoots(args[1:]))
	case "submit-roots":
		os.Exit(runProjectSubmitRoots(args[1:]))
	case "materialize":
		os.Exit(runProjectMaterialize(args[1:]))
	case "quote-step":
		os.Exit(runProjectQuoteStep(args[1:]))
	case "submit-step":
		os.Exit(runProjectSubmitStep(args[1:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown project subcommand %q\n", args[0])
		os.Exit(2)
	}
	return true
}

func runProjectQuoteRoots(args []string) int {
	fs := flag.NewFlagSet("project quote-roots", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory containing initial root inputs")
	irPath := fs.String("ir", "", "buyer-approved probed Project IR artifact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *irPath == "" {
		fmt.Fprintln(os.Stderr, "project quote-roots: --root and --ir are required")
		return 2
	}
	var ir ProjectWorkloadIR
	if err := readProjectArtifact(*irPath, &ir); err != nil {
		fmt.Fprintf(os.Stderr, "project quote-roots: invalid IR artifact: %v\n", err)
		return 1
	}
	quote, err := quoteInitialProjectRoots(newClient(), *root, ir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project quote-roots: %v\n", err)
		return 1
	}
	if err := writeProjectInitialQuote(os.Stdout, quote); err != nil {
		fmt.Fprintf(os.Stderr, "project quote-roots: %v\n", err)
		return 1
	}
	return 0
}

func runProjectSubmitRoots(args []string) int {
	fs := flag.NewFlagSet("project submit-roots", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory containing initial root inputs")
	irPath := fs.String("ir", "", "buyer-approved probed Project IR artifact")
	quotePath := fs.String("quote", "", "reviewed initial root quote artifact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *irPath == "" || *quotePath == "" {
		fmt.Fprintln(os.Stderr, "project submit-roots: --root, --ir and --quote are required")
		return 2
	}
	var ir ProjectWorkloadIR
	if err := readProjectArtifact(*irPath, &ir); err != nil {
		fmt.Fprintf(os.Stderr, "project submit-roots: invalid IR artifact: %v\n", err)
		return 1
	}
	var quote ProjectInitialQuote
	if err := readProjectArtifact(*quotePath, &quote); err != nil {
		fmt.Fprintf(os.Stderr, "project submit-roots: invalid initial root quote artifact: %v\n", err)
		return 1
	}
	result, err := submitInitialProjectRoots(newClient(), *root, ir, quote, time.Now())
	if writeErr := writeProjectSubmission(os.Stdout, result); writeErr != nil {
		fmt.Fprintf(os.Stderr, "project submit-roots: %v\n", writeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "project submit-roots: %v\n", err)
		return 1
	}
	return 0
}

func runProjectQuoteStep(args []string) int {
	fs := flag.NewFlagSet("project quote-step", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory containing materialized artifacts")
	irPath := fs.String("ir", "", "buyer-approved probed Project IR artifact")
	projectID := fs.String("project-id", "", "server project order UUID")
	stepID := fs.String("step", "", "declared dependent project step id")
	var materializations projectArtifactPaths
	fs.Var(&materializations, "materialization", "receipt-bound upstream materialization artifact (repeat once per dependency)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *irPath == "" || *projectID == "" || *stepID == "" || len(materializations) == 0 {
		fmt.Fprintln(os.Stderr, "project quote-step: --root, --ir, --project-id, --step and --materialization are required")
		return 2
	}
	var ir ProjectWorkloadIR
	if err := readProjectArtifact(*irPath, &ir); err != nil {
		fmt.Fprintf(os.Stderr, "project quote-step: invalid IR artifact: %v\n", err)
		return 1
	}
	receipts := make([]ProjectMaterialization, 0, len(materializations))
	for _, path := range materializations {
		var receipt ProjectMaterialization
		if err := readProjectArtifact(path, &receipt); err != nil {
			fmt.Fprintf(os.Stderr, "project quote-step: invalid materialization artifact: %v\n", err)
			return 1
		}
		receipts = append(receipts, receipt)
	}
	quote, err := quoteDependentProjectStep(newClient(), *root, ir, *projectID, *stepID, receipts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project quote-step: %v\n", err)
		return 1
	}
	if err := writeProjectDependentQuote(os.Stdout, quote); err != nil {
		fmt.Fprintf(os.Stderr, "project quote-step: %v\n", err)
		return 1
	}
	return 0
}

func runProjectSubmitStep(args []string) int {
	fs := flag.NewFlagSet("project submit-step", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory containing materialized artifacts")
	irPath := fs.String("ir", "", "buyer-approved probed Project IR artifact")
	quotePath := fs.String("quote", "", "reviewed dependent project quote artifact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || *irPath == "" || *quotePath == "" {
		fmt.Fprintln(os.Stderr, "project submit-step: --root, --ir and --quote are required")
		return 2
	}
	var ir ProjectWorkloadIR
	if err := readProjectArtifact(*irPath, &ir); err != nil {
		fmt.Fprintf(os.Stderr, "project submit-step: invalid IR artifact: %v\n", err)
		return 1
	}
	var quote ProjectDependentQuote
	if err := readProjectArtifact(*quotePath, &quote); err != nil {
		fmt.Fprintf(os.Stderr, "project submit-step: invalid dependent quote artifact: %v\n", err)
		return 1
	}
	result, err := submitDependentProjectStep(newClient(), *root, ir, quote, time.Now())
	if writeErr := writeProjectSubmission(os.Stdout, result); writeErr != nil {
		fmt.Fprintf(os.Stderr, "project submit-step: %v\n", writeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "project submit-step: %v\n", err)
		return 1
	}
	return 0
}

func writeProjectDependentQuote(w io.Writer, quote ProjectDependentQuote) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(quote)
}

func writeProjectInitialQuote(w io.Writer, quote ProjectInitialQuote) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(quote)
}

func runProjectQuote(args []string) int {
	fs := flag.NewFlagSet("project quote", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "project directory to compile and quote")
	approved := fs.String("buyer-approved-ir-sha256", "", "exact unprobed IR digest approved by the buyer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" || !validSHA256(*approved) {
		fmt.Fprintln(os.Stderr, "project quote: --root and --buyer-approved-ir-sha256 are required")
		return 2
	}
	ir, err := compileProject(projectCompileOptions{Root: *root, ProbeRequested: true, BuyerApprovedIRSHA256: *approved})
	if err != nil {
		fmt.Fprintf(os.Stderr, "project quote: %v\n", err)
		return 1
	}
	quote, err := quoteCompiledProject(newClient(), *root, ir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "project quote: %v\n", err)
		return 1
	}
	if err := writeProjectQuote(os.Stdout, quote); err != nil {
		fmt.Fprintf(os.Stderr, "project quote: %v\n", err)
		return 1
	}
	return 0
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
