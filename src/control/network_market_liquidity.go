package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NetworkMarketLiquidityReceipt is the bounded operator projection of the two
// live market lanes. It composes their immutable offer samples, demand
// outcomes, capacity fill, churn, and price depth; it is not a global market
// claim and never becomes pricing or settlement authority.
type NetworkMarketLiquidityReceipt struct {
	Version     int                                `json:"version"`
	ObservedAt  time.Time                          `json:"observed_at"`
	WindowStart time.Time                          `json:"window_start"`
	WindowEnd   time.Time                          `json:"window_end"`
	Window      string                             `json:"window"`
	MarketScope string                             `json:"market_scope"`
	Realtime    RealtimeMarketLiquidityReceipt     `json:"realtime"`
	Services    ServiceLeaseMarketLiquidityReceipt `json:"service_leases"`
}

func (s *Store) NetworkMarketLiquidity(ctx context.Context, window time.Duration) (NetworkMarketLiquidityReceipt, error) {
	if window <= 0 || window > maxRealtimeLiquidityWindow {
		return NetworkMarketLiquidityReceipt{}, fmt.Errorf("network liquidity window must be in (0,%s]", maxRealtimeLiquidityWindow)
	}
	realtime, err := s.RealtimeMarketLiquidity(ctx, window)
	if err != nil {
		return NetworkMarketLiquidityReceipt{}, err
	}
	services, err := s.ServiceLeaseMarketLiquidity(ctx, window)
	if err != nil {
		return NetworkMarketLiquidityReceipt{}, err
	}
	now := time.Now().UTC()
	return NetworkMarketLiquidityReceipt{
		Version: 1, ObservedAt: now, WindowStart: now.Add(-window), WindowEnd: now,
		Window:      window.String(),
		MarketScope: "MERC_RETAINED_REALTIME_AND_WARM_SERVICE_LANES_ONLY_NO_GLOBAL_OR_LEGAL_REGION_CLAIM",
		Realtime:    realtime, Services: services,
	}, nil
}

func (s *Server) handleAdminNetworkMarketLiquidity(w http.ResponseWriter, r *http.Request) {
	window := defaultRealtimeLiquidityWindow
	if raw := strings.TrimSpace(r.URL.Query().Get("window_seconds")); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seconds <= 0 {
			writeErr(w, http.StatusBadRequest, "window_seconds must be a positive integer")
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	receipt, err := s.store.NetworkMarketLiquidity(r.Context(), window)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}
