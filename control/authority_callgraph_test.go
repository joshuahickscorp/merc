package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A call graph, because an identifier scan is not one.
//
// TestCalibrationIsUnreachableFromMoneyAndAdmissionPaths asks "does this file's
// code name this symbol", which is the right question for a file that must never
// touch calibration and the wrong one for a file that is merely ALLOWED to. Two
// of the six allowlisted files are api.go and workers.go, which between them hold
// most of the control plane. A money handler in api.go calling a calibration
// reader in api.go passes that test completely, because the check is per file and
// api.go is on the list.
//
// This closes that hole by following calls instead of names: it builds the
// package's call graph from the AST and fails if any function declared in a
// money, pricing, settlement or admission file can reach a calibration or
// overhead READ, at any depth.
//
// Reads, not writes. Recording an observation from the finalize hook is the
// point of the table; the invariant is that no decision about money or admission
// may CONSUME one.
//
// No SSA and no golang.org/x/tools. This module has five direct dependencies and
// ships in the production container; pulling a program-analysis toolchain into
// go.mod and the SBOM for a test is a worse trade than writing the traversal.
// Everything here is one package, so a call resolves by name, which is the case
// SSA would mostly be buying.
//
// What this does NOT prove, stated here rather than left to be discovered:
//
//   - It is a call graph, not a data-flow analysis. A handler outside the guarded
//     files could read a calibration number and pass it in as an argument, and no
//     reachability check can see that — the receiving function cannot distinguish
//     it from any other input. Closing that needs taint tracking, which is a
//     different and much larger tool.
//   - Reflection and dynamically-built dispatch are invisible. Neither appears on
//     these paths today.
//   - Method calls resolve by bare name across every receiver type, so the graph
//     over-approximates. That direction is safe: it can report a path that no
//     concrete type actually takes, never miss one that does.
//
// Verified by mutation, not by inspection: planting
// `mutantPricingEntryPoint → mutantHelperReadsCalibration → Store.PlanAccuracy`
// in pricing.go fails this test with the full chain printed, and the identifier
// gate alone does not catch the two-hop form when the helper sits in an
// allowlisted file.

// callGraph maps a function key to the keys it can reach in one step.
type callGraph struct {
	// edges is keyed by "Recv.Name" for methods and "Name" for functions.
	// Built from every identifier that names a declared function (calls and
	// func values). Used for money-authority observation: a sink handed off as
	// a value is still reachable.
	edges map[string]map[string]bool
	// callEdges follows only CallExpr sites. Used for the calibration
	// consumption check so HTTP route registration (passing a handler method
	// value into mux.Handle) is not treated as the router consuming what the
	// handler reads. A real multi-hop call chain still appears here.
	callEdges map[string]map[string]bool
	// byName indexes every declaration sharing a bare name, so a call through a
	// receiver whose concrete type is not resolved here reaches all candidates.
	// Over-approximating edges can only make this gate stricter.
	byName map[string][]string
	// file records where each declaration lives.
	file map[string]string
}

// callExprFuncName returns the bare function/method name of a call's Fun, or "".
func callExprFuncName(fun ast.Expr) string {
	switch node := fun.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return node.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return callExprFuncName(node.X)
	case *ast.ParenExpr:
		return callExprFuncName(node.X)
	}
	return ""
}

func declKey(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	return receiverTypeName(decl.Recv.List[0].Type) + "." + decl.Name.Name
}

func receiverTypeName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(node.X)
	case *ast.Ident:
		return node.Name
	case *ast.IndexExpr: // generic receiver
		return receiverTypeName(node.X)
	}
	return "?"
}

// buildCallGraph parses every non-test Go file in the package.
func buildCallGraph(t *testing.T) *callGraph {
	t.Helper()
	graph := &callGraph{
		edges:     map[string]map[string]bool{},
		callEdges: map[string]map[string]bool{},
		byName:    map[string][]string{},
		file:      map[string]string{},
	}
	entries, err := filepath.Glob("*.go")
	must(t, err)
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		mustf(t, err, "parse %s: %v", name)
		parsed[name] = file
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := declKey(fn)
			graph.file[key] = name
			graph.byName[fn.Name.Name] = append(graph.byName[fn.Name.Name], key)
		}
	}

	for name, file := range parsed {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			from := declKey(fn)
			if graph.edges[from] == nil {
				graph.edges[from] = map[string]bool{}
			}
			if graph.callEdges[from] == nil {
				graph.callEdges[from] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// Ident/selector edges: every identifier that names a declared
				// function, whether called or passed as a value.
				id, ok := n.(*ast.Ident)
				if !ok {
					if sel, isSel := n.(*ast.SelectorExpr); isSel {
						id = sel.Sel
					} else if call, isCall := n.(*ast.CallExpr); isCall {
						// CallExpr edges: only actual invocations.
						if cname := callExprFuncName(call.Fun); cname != "" {
							for _, target := range graph.byName[cname] {
								if target != from {
									graph.callEdges[from][target] = true
								}
							}
						}
						return true
					} else {
						return true
					}
				}
				for _, target := range graph.byName[id.Name] {
					if target != from {
						graph.edges[from][target] = true
					}
				}
				return true
			})
			_ = name
		}
	}
	return graph
}

// reaches returns the shortest path from `from` to any key in `targets`, or nil,
// following identifier edges (calls and func values).
func (g *callGraph) reaches(from string, targets map[string]bool) []string {
	return g.reachesOn(from, targets, g.edges)
}

// reachesByCall is reaches restricted to CallExpr edges. Prefer this when
// asking whether a money path consumes a calibration read: route tables that
// merely register an admin handler must not count as consumption.
func (g *callGraph) reachesByCall(from string, targets map[string]bool) []string {
	return g.reachesOn(from, targets, g.callEdges)
}

func (g *callGraph) reachesOn(from string, targets map[string]bool, edges map[string]map[string]bool) []string {
	type step struct {
		key  string
		path []string
	}
	seen := map[string]bool{from: true}
	queue := []step{{key: from, path: []string{from}}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if targets[current.key] && current.key != from {
			return current.path
		}
		next := make([]string, 0, len(edges[current.key]))
		for key := range edges[current.key] {
			next = append(next, key)
		}
		sort.Strings(next) // deterministic path in the failure message
		for _, key := range next {
			if seen[key] {
				continue
			}
			seen[key] = true
			queue = append(queue, step{key: key, path: append(append([]string{}, current.path...), key)})
		}
	}
	return nil
}

// calibrationReadFunctions are the functions that CONSUME an observation.
//
// The recorders are deliberately absent: RecordPlanActuals, recordPlanActuals
// and RecordExecutionOverhead write, and writing from the finalize hook is what
// the tables are for. JobsMissingOverheadActuals reads the jobs table to find
// work for the recorder, not the observations, so it is not a consumer either.
var calibrationReadFunctions = []string{
	"Store.ResolvePlanCalibration",
	"Store.PlanAccuracy",
	"CalibrationPromotable",
	"Store.ExecutionOverhead",
	"Store.ExecutionOverheadHealth",
}

// moneyAndAdmissionAuthorityFiles own a decision about money or about whether
// work may be admitted or placed. Kept in step with the guarded list in
// plan_calibration_test.go by TestGuardedFileListsAgree.
//
// LEGACY filename list — retained for dual-run union with the structural
// money-authority observation (see money_authority_guard_test.go). Roots for
// TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads are
// filename-roots ∪ structural-authority-roots. Do not remove this list until
// dual-run has been green long enough to prove the structural view alone is
// sufficient; removal is remaining work, not done in this change.
var moneyAndAdmissionAuthorityFiles = []string{
	"billing.go", "buyer.go", "buyer_charge_operations.go", "collect.go",
	"economic_plan.go", "economic_facts.go", "ledger_write.go", "payment.go",
	"payment_authority.go", "prepaid.go", "pricing.go", "pricing_decision.go",
	"pricing_governance.go", "quote.go", "compute_plan.go", "scheduler.go",
	"realtime_placement.go", "shape_routing.go", "supplier_accrual.go",
	"stripe_settlement.go", "observed_output_settlement.go", "store_billing.go",
}

func TestNoCallPathFromMoneyOrAdmissionIntoCalibrationReads(t *testing.T) {
	graph := buildCallGraph(t)

	targets := map[string]bool{}
	for _, name := range calibrationReadFunctions {
		if _, declared := graph.edges[name]; !declared {
			t.Fatalf("%s is not a declared function; this gate is guarding a name "+
				"that no longer exists, which is the same as guarding nothing", name)
		}
		targets[name] = true
	}

	// Dual-run: prove every legacy filename still exists, then root at the
	// union of filename roots and structural money-authority reachability.
	for _, name := range moneyAndAdmissionAuthorityFiles {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("guarded file %s does not exist: %v", name, err)
		}
	}

	fileRoots := filenameMoneyAuthorityRoots(t, graph)
	structuralRoots := structuralMoneyAuthorityRoots(t, graph)
	roots := unionMoneyAuthorityRoots(t, graph)
	if len(roots) == 0 {
		t.Fatal("no money/admission authority roots; the traversal is vacuous")
	}

	for _, root := range roots {
		// CallExpr edges only: registration of an admin handler is not consumption.
		if path := graph.reachesByCall(root, targets); path != nil {
			t.Errorf("%s (%s) reaches calibration read %s:\n  %s",
				root, graph.file[root], path[len(path)-1], strings.Join(path, "\n  → "))
		}
	}
	t.Logf("checked %d roots (filename=%d structural=%d union=%d; dual-run) against %d calibration reads",
		len(roots), len(fileRoots), len(structuralRoots), len(roots), len(targets))
}

// The two gates must guard the same set of files, or tightening one silently
// leaves a hole in the other.
func TestGuardedFileListsAgree(t *testing.T) {
	// The identifier gate's list, copied here so a divergence is a test failure
	// rather than something a reader has to notice.
	identifierGate := []string{
		"billing.go", "buyer.go", "buyer_charge_operations.go", "collect.go",
		"economic_plan.go", "economic_facts.go", "ledger_write.go", "payment.go",
		"payment_authority.go", "prepaid.go", "pricing.go", "pricing_decision.go",
		"pricing_governance.go", "quote.go", "compute_plan.go", "scheduler.go",
		"realtime_placement.go", "shape_routing.go", "supplier_accrual.go",
		"stripe_settlement.go", "observed_output_settlement.go", "store_billing.go",
	}
	got := append([]string(nil), moneyAndAdmissionAuthorityFiles...)
	want := append([]string(nil), identifierGate...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the call-graph gate guards\n  %v\nand the identifier gate guards\n  %v",
			got, want)
	}
}

// The traversal must actually find a path when one exists, or a green result
// means nothing. This plants the exact shape the identifier gate cannot see: a
// guarded file calling a helper in an ALLOWED file that reads calibration.
func TestCallGraphFindsAnIndirectPath(t *testing.T) {
	graph := buildCallGraph(t)

	// A real edge that exists in the tree: whatever calls the resolver.
	callers := 0
	for from, to := range graph.edges {
		if to["Store.ResolvePlanCalibration"] {
			callers++
			t.Logf("%s (%s) calls the calibration resolver", from, graph.file[from])
		}
	}

	// Synthesize the two-hop shape and confirm the traversal reports it.
	graph.edges["fake.MoneyHandler"] = map[string]bool{"fake.Helper": true}
	graph.edges["fake.Helper"] = map[string]bool{"Store.PlanAccuracy": true}
	path := graph.reaches("fake.MoneyHandler", map[string]bool{"Store.PlanAccuracy": true})
	if len(path) != 3 {
		t.Fatalf("two-hop path was reported as %v; the traversal does not follow calls", path)
	}

	// And a function that reaches nothing must report nothing.
	graph.edges["fake.Isolated"] = map[string]bool{}
	if path := graph.reaches("fake.Isolated", map[string]bool{"Store.PlanAccuracy": true}); path != nil {
		t.Fatalf("an isolated function reported a path: %v", path)
	}
}
