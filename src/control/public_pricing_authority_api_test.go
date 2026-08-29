package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func currentPublicPricingFixture(
	t *testing.T,
) (context.Context, *Server, *Store, *pgxpool.Pool, CataloguePriceSchedule) {
	t.Helper()
	withActivationRestored(t)
	installSettlementCurrencyForTest(t, "usd")
	installBoundCataloguePublicationAuthorityForTest(t)
	pinBoardClockForPublication(t)
	ctx, store, pool := openIsolatedTestStore(t)
	schedule, err := BuildCataloguePriceSchedule()
	mustf(t, err, "build current public catalogue fixture: %v")
	_, err = store.ApplyRepricing(ctx, schedule)
	mustf(t, err, "apply current public catalogue fixture: %v")
	return ctx, NewServer(store, nil, nil, nil), store, pool, schedule
}

func callPublicPricingHandler(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	switch {
	case path == "/v1/models":
		server.handleModels(recorder, req)
	case strings.HasPrefix(path, "/v1/price-estimate"):
		server.handlePriceEstimate(recorder, req)
	case path == "/pricing/board.json":
		server.handlePriceBoardData(recorder, req)
	default:
		t.Fatalf("unknown pricing handler path %q", path)
	}
	return recorder
}

func assertPublicPricingUnavailable(
	t *testing.T,
	server *Server,
	modelID string,
	forbiddenNumbers ...float64,
) {
	t.Helper()
	paths := []string{
		"/v1/models",
		"/v1/price-estimate?model=" + modelID + "&units=1000&tier=batch",
		"/pricing/board.json",
	}
	for _, path := range paths {
		recorder := callPublicPricingHandler(t, server, path)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status=%d want 503 body=%s", path, recorder.Code, recorder.Body.String())
			continue
		}
		body := recorder.Body.String()
		if strings.Contains(body, "price_per_1k") || strings.Contains(body, "catalogue\"") ||
			strings.Contains(body, "market_evidence") {
			t.Errorf("%s exposed stale pricing fields on refusal: %s", path, body)
		}
		for _, forbidden := range forbiddenNumbers {
			text := strconv.FormatFloat(forbidden, 'f', -1, 64)
			if strings.Contains(body, text) {
				t.Errorf("%s exposed forbidden price %s on refusal: %s", path, text, body)
			}
		}
	}
}

func scheduleResultForModel(t *testing.T, schedule CataloguePriceSchedule, modelID string) RepriceResult {
	t.Helper()
	for _, result := range schedule.Results {
		if result.ModelID == modelID {
			return result
		}
	}
	t.Fatalf("schedule has no result for %s", modelID)
	return RepriceResult{}
}

func TestPublicPricingHandlersUseOnlyCurrentRevalidatedSchedule(t *testing.T) {
	_, server, _, _, schedule := currentPublicPricingFixture(t)
	if len(schedule.Results) == 0 {
		t.Fatal("fixture schedule has no results")
	}
	modelID := schedule.Results[0].ModelID
	want := schedule.Results[0].PricePer1K

	models := callPublicPricingHandler(t, server, "/v1/models")
	if models.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", models.Code, models.Body.String())
	}
	var modelBody struct {
		Data []ModelInfo `json:"data"`
	}
	mustf(t, json.Unmarshal(models.Body.Bytes(), &modelBody), "decode models response: %v")
	seen := false
	for _, model := range modelBody.Data {
		result := scheduleResultForModel(t, schedule, model.ID)
		if model.PricePer1K != result.PricePer1K || model.Currency != schedule.SettlementCurrency {
			t.Fatalf("model %s public price=%v %s want schedule=%v %s",
				model.ID, model.PricePer1K, model.Currency, result.PricePer1K, schedule.SettlementCurrency)
		}
		seen = seen || model.ID == modelID
	}
	if !seen {
		t.Fatalf("models response omitted governed model %s", modelID)
	}

	estimate := callPublicPricingHandler(t, server,
		"/v1/price-estimate?model="+modelID+"&units=1000&tier=batch")
	if estimate.Code != http.StatusOK {
		t.Fatalf("estimate status=%d body=%s", estimate.Code, estimate.Body.String())
	}
	var estimateBody PriceEstimate
	mustf(t, json.Unmarshal(estimate.Body.Bytes(), &estimateBody), "decode estimate: %v")
	if estimateBody.PricePer1K != want || estimateBody.Estimate != roundUSD(want) {
		t.Fatalf("estimate used price=%v total=%v want schedule=%v total=%v",
			estimateBody.PricePer1K, estimateBody.Estimate, want, roundUSD(want))
	}

	board := callPublicPricingHandler(t, server, "/pricing/board.json")
	if board.Code != http.StatusOK {
		t.Fatalf("public board status=%d body=%s", board.Code, board.Body.String())
	}
	var boardBody struct {
		SchemaVersion  int `json:"schema_version"`
		PriceAuthority struct {
			Status         string                    `json:"status"`
			ScheduleSHA256 string                    `json:"schedule_sha256"`
			Catalogue      []CataloguePriceAuthority `json:"catalogue"`
		} `json:"price_authority"`
		MarketEvidence struct {
			Status                string          `json:"status"`
			AuthorizesBuyerPrices bool            `json:"authorizes_buyer_prices"`
			SHA256                string          `json:"sha256"`
			Board                 json.RawMessage `json:"board"`
		} `json:"market_evidence"`
	}
	mustf(t, json.Unmarshal(board.Body.Bytes(), &boardBody), "decode public board: %v")
	if boardBody.SchemaVersion != 2 || boardBody.PriceAuthority.Status != "current_revalidated" ||
		boardBody.PriceAuthority.ScheduleSHA256 != schedule.SHA256 {
		t.Fatalf("public board omitted current schedule identity: %+v", boardBody.PriceAuthority)
	}
	if boardBody.MarketEvidence.Status != "evidence_only_not_buyer_authority" ||
		boardBody.MarketEvidence.AuthorizesBuyerPrices ||
		boardBody.MarketEvidence.SHA256 != schedule.BoardSHA256 || len(boardBody.MarketEvidence.Board) == 0 {
		t.Fatalf("raw board was not digest-bound evidence-only data: %+v", boardBody.MarketEvidence)
	}
	for _, authority := range boardBody.PriceAuthority.Catalogue {
		result := scheduleResultForModel(t, schedule, authority.ModelID)
		if authority.SettlementPricePer1K != result.PricePer1K {
			t.Fatalf("board authority %s price=%v want schedule=%v",
				authority.ModelID, authority.SettlementPricePer1K, result.PricePer1K)
		}
	}
}

func TestPublicPricingHandlersRefuseUnavailablePointerAndRawModelPrice(t *testing.T) {
	ctx, server, _, pool, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	const attackerPrice = 987654.321
	_, err := pool.Exec(ctx, `
		UPDATE models
		   SET price_source='seed', price_per_1k=$2
		 WHERE id=$1`, result.ModelID, attackerPrice)
	mustf(t, err, "break current catalogue pointer: %v")
	assertPublicPricingUnavailable(t, server, result.ModelID, result.PricePer1K, attackerPrice)
}

func TestPublicPricingHandlersRefuseExpiredPhysicalAuthority(t *testing.T) {
	_, server, _, _, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	boundary, err := time.Parse(time.RFC3339, result.PhysicalAuthority.Power.ValidUntil)
	mustf(t, err, "parse power validity boundary: %v")
	previous := cataloguePowerNow
	cataloguePowerNow = func() time.Time { return boundary.Add(time.Nanosecond) }
	t.Cleanup(func() { cataloguePowerNow = previous })
	assertPublicPricingUnavailable(t, server, result.ModelID, result.PricePer1K)
}

func TestPublicPricingHandlersRefuseWithdrawnPhysicalAuthority(t *testing.T) {
	_, server, _, _, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	path, _, _ := strings.Cut(result.PhysicalAuthority.Throughput.Citation, "#")
	raw, err := os.ReadFile(path)
	mustf(t, err, "read throughput authority: %v")
	var receipt map[string]any
	mustf(t, json.Unmarshal(raw, &receipt), "decode throughput authority: %v")
	receipt["binding_status"] = BindingWithdrawn
	mutated, err := json.MarshalIndent(receipt, "", "  ")
	mustf(t, err, "encode withdrawn throughput authority: %v")
	mustf(t, os.WriteFile(path, append(mutated, '\n'), 0o600), "withdraw throughput authority: %v")
	assertPublicPricingUnavailable(t, server, result.ModelID, result.PricePer1K)
}

func TestPublicPriceBoardRefusesRawBoardDigestDrift(t *testing.T) {
	_, server, _, _, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	resolved, err := resolvePriceBoard(os.Getenv("MERC_ENV"))
	mustf(t, err, "resolve current board: %v")
	original, err := os.ReadFile(resolved.Path)
	mustf(t, err, "read current board: %v")
	copyPath := t.TempDir() + "/mutated-board.json"
	mutated := append(append([]byte(nil), original...), ' ')
	mustf(t, os.WriteFile(copyPath, mutated, 0o600), "write mutated board: %v")
	t.Setenv(priceBoardPathEnv, copyPath)
	t.Setenv(priceBoardDigestEnv, "")

	recorder := callPublicPricingHandler(t, server, "/pricing/board.json")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("digest-drift board status=%d want 503 body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), strconv.FormatFloat(result.PricePer1K, 'f', -1, 64)) ||
		strings.Contains(recorder.Body.String(), "market_evidence") {
		t.Fatalf("digest-drift response exposed stale authority or raw evidence: %s", recorder.Body.String())
	}
}

func TestPublicPricingHandlersRefreshReplicaActivationBeforePublishing(t *testing.T) {
	ctx, server, store, _, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	stale, err := store.activationForNewAdmission(ctx)
	mustf(t, err, "capture pre-containment activation: %v")
	var containment []ActivationPolicyEntry
	for _, capability := range stale.advertised {
		if capability.Model == result.ModelID {
			containment = append(containment, ActivationPolicyEntry{
				RuntimeProfileID: capability.Runtime,
				CellID:           capability.ID,
				Lifecycle:        runtimeLifecycleQuarantined,
			})
		}
	}
	if len(containment) == 0 {
		t.Fatalf("no advertised cells found for %s", result.ModelID)
	}
	_, err = store.ApplyActivationPolicy(ctx, containment, "public pricing containment test")
	mustf(t, err, "commit cross-replica containment: %v")
	// Emulate a process that did not observe the writer's in-memory adoption.
	// The public handlers must refresh from PostgreSQL before consulting the
	// advertised projection.
	activeRuntimeActivation.Store(stale)

	models := callPublicPricingHandler(t, server, "/v1/models")
	if models.Code != http.StatusOK {
		t.Fatalf("models after containment status=%d body=%s", models.Code, models.Body.String())
	}
	var modelBody struct {
		Data []ModelInfo `json:"data"`
	}
	mustf(t, json.Unmarshal(models.Body.Bytes(), &modelBody), "decode contained models response: %v")
	for _, model := range modelBody.Data {
		if model.ID == result.ModelID {
			t.Fatalf("stale replica published quarantined model: %+v", model)
		}
	}

	estimate := callPublicPricingHandler(t, server,
		"/v1/price-estimate?model="+result.ModelID+"&units=1000&tier=batch")
	if estimate.Code != http.StatusBadRequest || strings.Contains(estimate.Body.String(), "price_per_1k") {
		t.Fatalf("quarantined model estimate status=%d body=%s", estimate.Code, estimate.Body.String())
	}

	board := callPublicPricingHandler(t, server, "/pricing/board.json")
	if board.Code != http.StatusOK {
		t.Fatalf("board after partial containment status=%d body=%s", board.Code, board.Body.String())
	}
	var boardBody struct {
		PriceAuthority struct {
			Catalogue []CataloguePriceAuthority `json:"catalogue"`
		} `json:"price_authority"`
	}
	mustf(t, json.Unmarshal(board.Body.Bytes(), &boardBody), "decode contained board response: %v")
	for _, authority := range boardBody.PriceAuthority.Catalogue {
		if authority.ModelID == result.ModelID {
			t.Fatalf("stale replica board published quarantined authority: %+v", authority)
		}
	}
}

func TestPublicPricingHandlersFail503WhenActivationDatabaseUnavailable(t *testing.T) {
	_, server, _, pool, schedule := currentPublicPricingFixture(t)
	result := schedule.Results[0]
	pool.Close()
	assertPublicPricingUnavailable(t, server, result.ModelID, result.PricePer1K)
}
