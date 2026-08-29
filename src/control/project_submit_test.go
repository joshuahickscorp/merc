package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func executableProjectQuoteFixture(t *testing.T, root string, now time.Time, handler func(cliJobSubmit, string)) (ProjectWorkloadIR, ProjectQuote, *httptest.Server) {
	t.Helper()
	input := []byte("{\"text\":\"hello\"}\n")
	writeProjectFixture(t, root, "input.jsonl", string(input))
	serverQuote := validProjectServerQuote(t)
	serverQuote.QuoteID = "q_" + uuid.NewString()
	serverQuote.Tier = "batch"
	serverQuote.ExpiresAt = now.Add(time.Hour)
	digest := sha256.Sum256(input)
	serverQuote.InputSHA256 = hex.EncodeToString(digest[:])
	currency, err := ParseCurrency(serverQuote.Currency)
	must(t, err)
	maximum, err := MoneyNanosFromUSDFloat(currency, serverQuote.Cost.MaxUSD)
	must(t, err)
	ir := projectQuoteIRFixture(serverQuote, maximum.Nanos+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/quote":
			writeJSON(w, http.StatusOK, serverQuote)
		case "/v1/projects":
			var order projectOrderCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
				t.Error(err)
				return
			}
			if order.IRSHA256 == "" || order.Currency != serverQuote.Currency || order.BuyerCeilingNanos <= 0 ||
				!strings.HasPrefix(r.Header.Get("Idempotency-Key"), "project-order:") {
				t.Errorf("project order lost its buyer ceiling authority: %+v", order)
			}
			writeJSON(w, http.StatusCreated, ProjectOrder{ID: uuid.NewString(), IRSHA256: order.IRSHA256,
				Currency: order.Currency, BuyerCeilingNanos: order.BuyerCeilingNanos,
				RemainingNanos: order.BuyerCeilingNanos, Status: "OPEN"})
		case "/v1/jobs":
			var request cliJobSubmit
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if handler != nil {
				handler(request, r.Header.Get("Idempotency-Key"))
			}
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusAccepted, JobSubmitResponse{JobID: uuid.New()})
		default:
			http.NotFound(w, r)
		}
	}))
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	artifact, err := quoteCompiledProject(c, root, ir)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return ir, artifact, server
}

func TestSubmitCompiledProjectPreservesReviewedAuthority(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var calls int
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, func(request cliJobSubmit, key string) {
		calls++
		if !request.FirmQuote || request.QuoteID == "" || request.ProjectID == "" || request.ProjectStepID == "" {
			t.Error("project submit did not bind a firm quote")
		}
		if !strings.HasPrefix(key, "project:") || len(key) > 128 {
			t.Errorf("invalid deterministic idempotency key %q", key)
		}
	})
	defer server.Close()
	c := &client{base: server.URL, key: "test-project-key", hc: server.Client()}
	result, err := submitCompiledProject(c, root, ir, artifact, now)
	must(t, err)
	if calls != 1 || result.Status != "ACCEPTED" || result.ExecutionMode != "INDEPENDENT_FINITE_STEPS" ||
		result.ProjectID == "" || len(result.Steps) != 1 || !result.Steps[0].IdempotentReplay ||
		result.Steps[0].QuoteID != artifact.Steps[0].QuoteID ||
		result.Steps[0].PricingDecisionSHA256 != artifact.Steps[0].PricingDecisionSHA256 {
		t.Fatalf("project submission lost reviewed authority: %+v calls=%d", result, calls)
	}
}

func TestSubmitCompiledProjectRefusesAuthorityTamperingBeforeMutation(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	var calls int
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, func(cliJobSubmit, string) { calls++ })
	defer server.Close()
	artifact.Steps[0].Authority.Pricing.BuyerPrice++
	_, err := submitCompiledProject(&client{base: server.URL, hc: server.Client()}, root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "authority quote digest mismatch") || calls != 0 {
		t.Fatalf("tampered quote reached mutation: err=%v calls=%d", err, calls)
	}
}

func TestSubmitCompiledProjectRefusesChangedInputAndDependencies(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, nil)
	defer server.Close()
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"changed\"}\n")
	_, err := validateProjectQuoteForSubmit(root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "input changed") {
		t.Fatalf("changed input passed: %v", err)
	}
	writeProjectFixture(t, root, "input.jsonl", "{\"text\":\"hello\"}\n")
	ir.Steps[0].DependsOn = []string{"upstream"}
	_, err = validateProjectQuoteForSubmit(root, ir, artifact, now)
	if err == nil || !strings.Contains(err.Error(), "only independent finite steps") {
		t.Fatalf("dependency graph was mislabeled executable: %v", err)
	}
}

func TestSubmitCompiledProjectRefusesExpiredQuote(t *testing.T) {
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	root := t.TempDir()
	ir, artifact, server := executableProjectQuoteFixture(t, root, now, nil)
	defer server.Close()
	_, err := validateProjectQuoteForSubmit(root, ir, artifact, now.Add(2*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "quote expired") {
		t.Fatalf("expired quote passed: %v", err)
	}
}

type projectCompilerDurableCounts struct {
	quotes, projects, projectSteps, jobs, tasks, ledger int64
}

func readProjectCompilerDurableCounts(t *testing.T, pool *pgxpool.Pool) projectCompilerDurableCounts {
	t.Helper()
	var out projectCompilerDurableCounts
	mustf(t, pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM quotes),
		       (SELECT count(*) FROM project_orders),
		       (SELECT count(*) FROM project_order_steps),
		       (SELECT count(*) FROM jobs),
		       (SELECT count(*) FROM tasks),
		       (SELECT count(*) FROM ledger_entries)`).Scan(
		&out.quotes, &out.projects, &out.projectSteps,
		&out.jobs, &out.tasks, &out.ledger,
	), "read project compiler durable counts: %v")
	return out
}

// assertProjectCompilerCADEmbedRefusesAtPublicQuote preserves the useful
// compiler proof without inventing conversion authority. The exact advertised
// runtime/model contracts still resolve and the bounded probe is still tied to
// buyer approval; the authenticated quote then refuses because completed
// embedding records and token-like input geometry are not interchangeable.
func assertProjectCompilerCADEmbedRefusesAtPublicQuote(
	t *testing.T, qualityRequirement, input string,
) {
	t.Helper()
	// Publish a catalogue under TEST_ONLY authority first (so quote is not
	// short-circuited by a missing price row), then re-bind the embed cell to
	// the legacy completed_embedding_records unit/scope. Settlement wants
	// token-like input geometry, so the public CAD quote must refuse conversion
	// without writes.
	strangerDeploymentInputs(t)
	installSettlementCurrencyForTest(t, "cad")
	installBoundCataloguePublicationAuthorityForTest(t)

	artifacts := newArtifactHarness(t)
	ctx, store, pool := openIsolatedTestStore(t)
	installed := currentActivation()
	activeRuntimeActivation.Store(newRuntimeActivation(
		installed.PolicyRevision, map[string]string{}, nil))
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build catalogue price schedule: %v")
	if _, err := store.ApplyRepricing(ctx, schedule); err != nil {
		t.Fatalf("publish catalogue price schedule: %v", err)
	}
	// After the schedule is durable, re-point the embed cell at the legacy
	// completed_embedding_records unit so performance settlement mismatches.
	installTestOnlyExactIdentityForLegacyBenchmark(t, candleEmbedCell)
	activeRuntimeActivation.Store(newRuntimeActivation(
		currentActivation().PolicyRevision, map[string]string{}, nil))
	mustf(t, seedDemo(ctx, pool, artifacts.storage), "seed verification floor: %v")
	server := httptest.NewServer(NewServer(store, artifacts.storage, nil, nil).Routes())
	t.Cleanup(server.Close)

	signup := postJSON(t, server.URL+"/v1/signup", "", map[string]any{
		"email": "project-" + uuid.NewString() + "@example.test", "password": "a-stranger-password-1234",
	})
	if signup.status != http.StatusOK && signup.status != http.StatusCreated {
		t.Fatalf("signup: HTTP %d: %s", signup.status, signup.body)
	}
	buyerKey, _ := signup.json["sandbox_key"].(string)
	if buyerKey == "" {
		t.Fatalf("signup issued no sandbox API key: %s", signup.body)
	}

	contracts, err := advertisedProjectRuntimeContracts()
	must(t, err)
	var embed ProjectRuntimeContract
	for _, candidate := range contracts {
		if candidate.WorkloadKind == "embeddings" {
			embed = candidate
			break
		}
	}
	if embed.RuntimeID == "" || embed.ModelID == "" {
		t.Fatal("no advertised embeddings runtime contract under exact-identity fixture")
	}

	root := t.TempDir()
	writeProjectFixture(t, root, "input.jsonl", input)
	writeProjectFixture(t, root, "pipeline.py", "embedding = client.embeddings.create(...)\n")
	writeDeclarationFixture(t, root, ProjectDeclaration{
		Version: 1,
		Steps: []ProjectIRStep{{
			ID: "embed", Kind: "embeddings", Inputs: []string{"project://input.jsonl"}, Outputs: []string{"project://vectors"},
			RuntimeContract: embed.RuntimeContractSHA256, ModelContract: embed.ModelContractSHA256,
			ResourceEstimate: ProjectIRResourceEstimate{State: "BOUNDED_PROBE_REQUIRED"}, Parallelism: "INDEPENDENT",
			CheckpointPolicy: "NOT_APPLICABLE", Verification: embed.Verification,
		}},
		Privacy: ProjectIRPrivacy{Egress: "DENY", DataLocation: "CA"},
		Quality: ProjectIRQuality{Requirement: qualityRequirement, Verification: "independent"},
		Result:  ProjectIRResult{Contract: "vectors-v1", Retention: "30d", Delivery: "object-store"},
		Economics: ProjectIREconomics{
			Currency: "cad", MaximumBuyerPriceNanos: 20_000_000_000,
			SupplierFloor: "UNRESOLVED_REFUSE", MercContribution: "UNRESOLVED_REFUSE",
		},
	})

	proposal, err := compileProject(projectCompileOptions{Root: root})
	mustf(t, err, "compile unprobed project: %v")
	ir, err := compileProject(projectCompileOptions{
		Root: root, ProbeRequested: true, BuyerApprovedIRSHA256: proposal.IRSHA256,
	})
	mustf(t, err, "compile buyer-approved probe: %v")
	if !ir.Probe.Executed || !ir.Probe.BuyerAuthorized || ir.Probe.ApprovedIRSHA256 != proposal.IRSHA256 {
		t.Fatalf("project probe was not bound to buyer approval: %+v", ir.Probe)
	}
	if len(ir.Steps) != 1 || ir.Steps[0].RuntimeID != embed.RuntimeID ||
		ir.Steps[0].ModelID != embed.ModelID ||
		ir.Steps[0].RuntimeContract != embed.RuntimeContractSHA256 ||
		ir.Steps[0].ModelContract != embed.ModelContractSHA256 {
		t.Fatalf("buyer-approved compiler IR lost the exact embed contract before the authority refusal: %+v", ir.Steps)
	}
	if ir.IRSHA256 == "" || strings.Contains(strings.Join(ir.RefusalReasons, "\n"), "runtime/model contract pair") {
		t.Fatalf("compiler failed before the intended physical-authority boundary: digest=%q refusals=%v", ir.IRSHA256, ir.RefusalReasons)
	}

	before := readProjectCompilerDurableCounts(t, pool)
	_, err = quoteCompiledProject(&client{base: server.URL, key: buyerKey, hc: server.Client()}, root, ir)
	if err == nil {
		t.Fatal("scope-incompatible embed project received a live CAD quote")
	}
	msg := err.Error()
	// Exact-identity rebind can surface either the unit/scope conversion refusal
	// or a catalogue physical re-resolution failure naming the legacy embeddings
	// unit path. Both are pre-write refusals of scope-incompatible authority.
	if !strings.Contains(msg, "503") && !strings.Contains(msg, "Service Unavailable") {
		t.Fatalf("project quote refusal is not 503: %v", err)
	}
	conversionNamed := strings.Contains(msg, performanceUnitScopeCompletedEmbeddingRecords) &&
		strings.Contains(msg, performanceUnitScopeTokenLikeInputGeometry) &&
		strings.Contains(msg, "no frozen unit conversion authority")
	physicalNamed := strings.Contains(msg, "physical authority") ||
		strings.Contains(msg, "unit/scope") ||
		strings.Contains(msg, "embeddings") ||
		strings.Contains(msg, candleEmbedCell)
	if !conversionNamed && !physicalNamed {
		t.Fatalf("project quote refusal does not name scope-incompatible authority: %v", err)
	}
	after := readProjectCompilerDurableCounts(t, pool)
	if after != before {
		t.Fatalf("refused embed project mutated durable state: before=%+v after=%+v", before, after)
	}
}

// TestProjectCompilerCADEmbedAdmissionRefusesWithoutScopeCompatibleAuthority
// replaces the former positive admission claim. The compiler may resolve the
// exact embed contract, but the public CAD quote must return 503 and write
// nothing until a governed Unit+UnitScope conversion exists.
func TestProjectCompilerCADEmbedAdmissionRefusesWithoutScopeCompatibleAuthority(t *testing.T) {
	assertProjectCompilerCADEmbedRefusesAtPublicQuote(t,
		"project-public-admission-refusal-v1",
		"{\"text\":\"the compiler may resolve a contract but must not derive conversion authority\"}\n"+
			"{\"text\":\"the public CAD quote must fail closed before admission\"}\n",
	)
}

// TestProjectCompilerCADEmbedExecutionRefusesBeforeDurableMutation replaces the
// former live-execution claim. There can be no current execution proof when no
// scope-compatible admission authority exists; the useful compiler/probe proof
// remains, and the zero-write check includes tasks and ledger rows.
func TestProjectCompilerCADEmbedExecutionRefusesBeforeDurableMutation(t *testing.T) {
	assertProjectCompilerCADEmbedRefusesAtPublicQuote(t,
		"project-public-execution-refusal-v1",
		"{\"text\":\"no project execution may begin without scope-compatible CAD authority\"}\n",
	)
}
