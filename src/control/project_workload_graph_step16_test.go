package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Step 16 close: EXACT REFUSAL.
//
// ProjectWorkloadIR is the proposal graph. WorkloadDecision is the single-job
// admission freeze. Every executable project path flattens to independently
// reclassified jobs after buyer-side dependency checks. Promoting the IR into a
// second accepted "Workload" graph authority beside buildWorkloadDecisionForSubmit
// would duplicate the admission freeze — the defect class this programme guards
// against hardest.
//
// These tests lock that refusal. They do not invent a graph-on-receipt product
// that no production consumer of multi-step structure requires today.

// projectGraphJSONTags are graph / multi-step identity fields owned by the
// project IR (and order / compile receipts). They must not appear on the
// single-job admission freeze or the assembled job receipt.
var projectGraphJSONTags = map[string]bool{
	"depends_on":        true,
	"ir_sha256":         true,
	"project_sha256":    true,
	"steps":             true,
	"artifacts":         true,
	"checkpoint_policy": true,
	"project_order_id":  true,
	"project_step_id":   true,
	"project_id":        true,
}

func jsonTagName(tag reflect.StructTag) string {
	raw := tag.Get("json")
	if raw == "" || raw == "-" {
		return ""
	}
	name, _, _ := strings.Cut(raw, ",")
	return name
}

// projectGraphFieldViolations walks exported struct fields and reports any
// JSON tag from projectGraphJSONTags. Used both as the production tripwire and
// as the fail-when-broken probe on a forged type.
func projectGraphFieldViolations(typ reflect.Type, path string) []string {
	if typ == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := jsonTagName(field.Tag)
		fieldPath := path + "." + field.Name
		if name != "" && projectGraphJSONTags[name] {
			out = append(out, fieldPath+" json:"+name)
		}
		ft := field.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() == typ.PkgPath() {
			// Stay inside this package; do not recurse into uuid.Time etc.
			out = append(out, projectGraphFieldViolations(ft, fieldPath)...)
		}
	}
	return out
}

func TestStep16TripwireCatchesInjectedGraphFields(t *testing.T) {
	// Failing-before: the same checker must fail if someone later promotes the
	// graph into WorkloadDecision or ClearingReceipt.
	type forgedDecision struct {
		DependsOn []string `json:"depends_on"`
		IRSHA256  string   `json:"ir_sha256"`
		Steps     []string `json:"steps"`
	}
	violations := projectGraphFieldViolations(reflect.TypeOf(forgedDecision{}), "forged")
	if len(violations) < 3 {
		t.Fatalf("tripwire failed to catch forged graph fields: %v", violations)
	}
}

func TestWorkloadDecisionIsSingleJobFreezeNotGraphProjection(t *testing.T) {
	violations := projectGraphFieldViolations(reflect.TypeOf(WorkloadDecision{}), "WorkloadDecision")
	if len(violations) != 0 {
		t.Fatalf("WorkloadDecision acquired project-graph fields (parallel authority): %v", violations)
	}
	// Binding is the request shape for one job; it also must not smuggle the graph.
	violations = projectGraphFieldViolations(reflect.TypeOf(WorkloadBinding{}), "WorkloadBinding")
	if len(violations) != 0 {
		t.Fatalf("WorkloadBinding acquired project-graph fields: %v", violations)
	}
}

func TestClearingReceiptDoesNotCarryProjectGraphIdentity(t *testing.T) {
	// A multi-step project's structure is not recoverable from the job receipt
	// alone: no ir_sha256, no project_order_id, no depends_on.
	for _, sample := range []struct {
		name string
		typ  reflect.Type
	}{
		{"ClearingReceipt", reflect.TypeOf(ClearingReceipt{})},
		{"ReceiptAuthority", reflect.TypeOf(ReceiptAuthority{})},
		{"JobStatus", reflect.TypeOf(JobStatus{})},
	} {
		if violations := projectGraphFieldViolations(sample.typ, sample.name); len(violations) != 0 {
			t.Fatalf("%s acquired project-graph fields: %v", sample.name, violations)
		}
	}
}

func TestNoCanonicalWorkloadGraphTypeBesideProposalAndDecision(t *testing.T) {
	// type Workload struct would be the second accepted graph authority the
	// shape note forbids. WorkloadDecision / WorkloadBinding / … are allowed.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse control package: %v", err)
	}
	var found []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok || ts.Name == nil || ts.Name.Name != "Workload" {
					return true
				}
				if _, isStruct := ts.Type.(*ast.StructType); isStruct {
					found = append(found, filepath.Base(path)+":"+ts.Name.Name)
				}
				return true
			})
		}
	}
	if len(found) != 0 {
		t.Fatalf("found canonical Workload graph type(s) beside ProjectWorkloadIR/WorkloadDecision: %v", found)
	}
}

func TestBuildWorkloadDecisionPathsDoNotIngestProjectIR(t *testing.T) {
	// Production freeze is buildWorkloadDecisionForSubmit(jobSubmit, inputSHA).
	// No freeze path may take ProjectWorkloadIR / ProjectIRStep as input — that
	// would be the "projection that re-runs as graph authority" trap.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "workload_classification.go", nil, 0)
	if err != nil {
		t.Fatalf("parse workload_classification.go: %v", err)
	}
	forbidden := map[string]bool{
		"ProjectWorkloadIR": true,
		"ProjectIRStep":     true,
		"ProjectOrder":      true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "buildWorkloadDecision") {
			return true
		}
		if fn.Type.Params == nil {
			return true
		}
		for _, field := range fn.Type.Params.List {
			name := typeName(field.Type)
			if forbidden[name] {
				t.Errorf("%s takes %s — graph must not feed the single-job freeze", fn.Name.Name, name)
			}
		}
		return true
	})
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return typeName(t.Sel)
	default:
		return ""
	}
}

func TestEvidenceEnvelopeWorkloadLinkCitesDecisionNotProjectIR(t *testing.T) {
	// Envelope workload stage binds WorkloadDecision, not the project graph.
	digest := strings.Repeat("a", 64)
	link, err := batchAcceptLink(EnvelopeLinkWorkload, batchAcceptBoundDigests{WorkloadSHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	if link.Status != EnvelopeLinkBound || link.Authority != "WorkloadDecision" || link.Digest != digest {
		t.Fatalf("envelope workload link must cite WorkloadDecision, got %+v", link)
	}
	if link.Authority == "ProjectWorkloadIR" || strings.Contains(strings.ToLower(link.Authority), "project") {
		t.Fatalf("envelope workload link must not cite project IR: %+v", link)
	}
}

func TestIndependentProjectSubmitRefusesDependencyGraphByConstruction(t *testing.T) {
	// Executable mode INDEPENDENT_FINITE_STEPS refuses any depends_on before
	// firm submit. Flattening is the admission model, not a temporary gap.
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, nil)
	defer server.Close()
	ir.Steps[0].DependsOn = []string{"upstream"}
	_, err := validateProjectQuoteForSubmit(root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "only independent finite steps") {
		t.Fatalf("dependency graph was mislabeled executable: %v", err)
	}
}

func TestDependentPathErasesDependsOnBeforeSingleJobAdmission(t *testing.T) {
	// Dependent graphs check materializations against the full IR, then erase
	// DependsOn on a one-step copy so validateProjectQuoteForSubmit / ordinary
	// classification stay single-job. The original IR keeps the edge.
	root, ir, materialization, c, _ := dependentProjectFixture(t)
	step, ok := findProjectIRStep(ir, "downstream")
	if !ok || len(step.DependsOn) == 0 {
		t.Fatalf("fixture lost declared dependency: %+v", ir.Steps)
	}
	quote, err := quoteDependentProjectStep(c, root, ir, materialization.ProjectID, "downstream", []ProjectMaterialization{materialization})
	if err != nil {
		t.Fatal(err)
	}
	// After quote, original IR is intact.
	still, ok := findProjectIRStep(ir, "downstream")
	if !ok || len(still.DependsOn) == 0 {
		t.Fatalf("dependent quote mutated the approved IR graph: %+v", ir.Steps)
	}
	// The firm path only admits the erased copy.
	ready := still
	ready.DependsOn = nil
	readyIR := ir
	readyIR.Steps = []ProjectIRStep{ready}
	projectQuote := ProjectQuote{
		Version: 2, IRSHA256: ir.IRSHA256, Currency: quote.Currency,
		ExpectedCostNanos: quote.Step.ExpectedCostNanos, MaximumCostNanos: quote.Step.MaximumCostNanos,
		BuyerCeilingNanos: quote.BuyerCeilingNanos, CriticalPathP50Secs: quote.Step.P50Secs,
		CriticalPathP90Secs: quote.Step.P90Secs, MinimumConfidence: quote.Step.Confidence,
		CalibrationState: quote.CalibrationState, Steps: []ProjectStepQuote{quote.Step},
	}
	prepared, err := validateProjectQuoteForSubmit(root, readyIR, projectQuote, time.Now())
	if err != nil || len(prepared) != 1 {
		t.Fatalf("erased one-step copy must be admissible: err=%v prepared=%d", err, len(prepared))
	}
	// Without erase, the same quote against the graph is refused.
	graphIR := ir
	graphIR.Steps = []ProjectIRStep{still}
	_, err = validateProjectQuoteForSubmit(root, graphIR, projectQuote, time.Now())
	if err == nil || !strings.Contains(err.Error(), "only independent finite steps") {
		t.Fatalf("graph with depends_on must not pass single-job submit validation: %v", err)
	}
}

func TestProjectIRHoldsGraphFieldsFlattenedJobsLose(t *testing.T) {
	// Positive control: the proposal type really does carry the structure the
	// job freeze lacks. If ProjectIRStep loses depends_on, the refusal premise
	// ("flattening loses the graph") is wrong and this step must be reopened.
	stepTags := map[string]bool{}
	st := reflect.TypeOf(ProjectIRStep{})
	for i := 0; i < st.NumField(); i++ {
		if name := jsonTagName(st.Field(i).Tag); name != "" {
			stepTags[name] = true
		}
	}
	for _, required := range []string{"id", "depends_on", "inputs", "outputs", "checkpoint_policy", "verification", "parallelism"} {
		if !stepTags[required] {
			t.Fatalf("ProjectIRStep missing required graph field %q", required)
		}
	}
	irTags := map[string]bool{}
	it := reflect.TypeOf(ProjectWorkloadIR{})
	for i := 0; i < it.NumField(); i++ {
		if name := jsonTagName(it.Field(i).Tag); name != "" {
			irTags[name] = true
		}
	}
	for _, required := range []string{"ir_sha256", "steps", "artifacts", "result", "economics", "privacy", "quality"} {
		if !irTags[required] {
			t.Fatalf("ProjectWorkloadIR missing required graph field %q", required)
		}
	}
}
