package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func validateHardeningSecretConfig(
	cxEnv, stripeSecret, tokenKey, verificationSampleSecret string,
) (bool, error) {
	tokenKeyUnsafe := len(tokenKey) < 32
	verificationUsesPublishedDefault := verificationSampleSecret == "" ||
		verificationSampleSecret == insecureDevelopmentSamplingSecret
	verificationSecretUnsafe := len(verificationSampleSecret) < 32 || verificationUsesPublishedDefault
	missing := tokenKeyUnsafe || verificationSecretUnsafe
	if !missing {
		return false, nil
	}
	// Any Stripe key (including sk_test_) refuses the published sampling default:
	// test money still buys fraud practice against a known sampling oracle.
	if verificationUsesPublishedDefault && strings.TrimSpace(stripeSecret) != "" {
		return true, fmt.Errorf(
			"MERC_VERIFICATION_SAMPLE_SECRET missing or is the published development default while STRIPE_SECRET_KEY is set  -  refusing to start: verification sampling would be unhardened with money rails present; set MERC_VERIFICATION_SAMPLE_SECRET to at least 32 unpredictable bytes",
		)
	}
	liveStripe := isLiveStripeCredential(stripeSecret)
	if isProductionEnv(cxEnv) || liveStripe {
		reason := "MERC_ENV=" + cxEnv
		if liveStripe {
			reason = "STRIPE_SECRET_KEY is a LIVE key (sk_live_...)"
		}
		return true, fmt.Errorf(
			"MERC_TOKEN_KEY and/or MERC_VERIFICATION_SAMPLE_SECRET missing or unsafe with %s  -  refusing to start: OAuth token storage, webhook signing-secret storage, or verification sampling would be unhardened; set both to at least 32 unpredictable bytes",
			reason,
		)
	}
	return true, nil
}

const (
	defaultDBMaxConns int32 = 20
	maxDBMaxConns     int64 = 1000
)

func parseDBMaxConns(raw string) (int32, error) {
	if raw == "" {
		return defaultDBMaxConns, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 || n > maxDBMaxConns {
		return 0, fmt.Errorf("DB_MAX_CONNS must be an integer from 1 to %d", maxDBMaxConns)
	}
	return int32(n), nil
}

func validateExecutionDBCapacity(maxConns int32, workerLeaderEnabled bool) error {
	leaderConns := int32(0)
	if workerLeaderEnabled {
		leaderConns = verificationWorkerLeaderConnections
	}
	required := leaderConns + verificationDBHeadroom + verificationDBConnectionsPerProcess
	if maxConns < required {
		return fmt.Errorf(
			"DB_MAX_CONNS=%d cannot safely run verification: need at least %d (%d worker leader + %d verifier + %d API/background headroom)",
			maxConns, required, leaderConns, verificationDBConnectionsPerProcess, verificationDBHeadroom,
		)
	}
	return nil
}

// isProductionEnv matches the same spellings the hardening refusal accepts.
func isProductionEnv(cxEnv string) bool {
	return strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod")
}

func validateSeedAllowed(cxEnv, stripeSecret string) error {
	production := isProductionEnv(cxEnv)
	if production || isLiveStripeCredential(stripeSecret) {
		return fmt.Errorf("control seed is disabled in production/live-money mode; refusing to install public development credentials")
	}
	return nil
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

func stripeCredentialClass(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "missing"
	case strings.HasPrefix(value, "sk_test_"):
		return "sk_test"
	case strings.HasPrefix(value, "sk_live_"):
		return "sk_live"
	case strings.HasPrefix(value, "rk_test_"):
		return "rk_test"
	case strings.HasPrefix(value, "rk_live_"):
		return "rk_live"
	case strings.HasPrefix(value, "pk_test_"):
		return "publishable_test"
	case strings.HasPrefix(value, "pk_live_"):
		return "publishable_live"
	case strings.HasPrefix(value, "whsec_"):
		return "webhook_present"
	default:
		return "unknown"
	}
}

func isLiveStripeCredential(value string) bool {
	switch stripeCredentialClass(value) {
	case "sk_live", "rk_live", "publishable_live":
		return true
	default:
		return false
	}
}

func validatePaymentProviderMode(cxEnv, provider string) error {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" || provider == "stripe" || provider == "none" {
		return nil
	}
	if provider == "simulator" {
		if strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod") ||
			strings.EqualFold(cxEnv, "staging") {
			return errors.New("test-only payment simulator is unavailable in production or staging")
		}
		return errors.New("test-only payment simulator is CLI/test-only and cannot back the server")
	}
	return fmt.Errorf("unsupported MERC_PAYMENT_PROVIDER %q", provider)
}

func validateCanaryMoneyMode(
	canaryRaw, stripeSecret, billingWebhookSecret, connectWebhookSecret, connectClientID, payoutExport string,
) error {
	enabled, err := strconv.ParseBool(strings.TrimSpace(canaryRaw))
	if err != nil && strings.TrimSpace(canaryRaw) != "" {
		return fmt.Errorf("MERC_CANARY_MODE must be a boolean")
	}
	if !enabled {
		return nil
	}
	if !strings.HasPrefix(stripeSecret, "sk_test_") && !strings.HasPrefix(stripeSecret, "rk_test_") {
		return fmt.Errorf("private canary requires STRIPE_SECRET_KEY=sk_test_* or scoped rk_test_*; live or absent keys are refused")
	}
	if strings.TrimSpace(payoutExport) != "" {
		return fmt.Errorf("private canary refuses MERC_PAYOUT_EXPORT; supplier value movement is test-mode only")
	}
	// A backend staging alpha that never onboarded Connect has no platform
	// client ID to name. Demanding ca_* / a Connect webhook there is the
	// self-serve-payout decision, not the private-canary money-rail check:
	// test-mode Stripe is still required, and the moment Connect return/refresh
	// URLs or a ca_* appear the full pair is required again. Production is
	// unchanged. MERC_ENV is read here rather than passed so existing callers
	// and tests stay production-shaped unless they opt into staging.
	connectConfigured := strings.HasPrefix(strings.TrimSpace(connectClientID), "ca_") ||
		strings.TrimSpace(os.Getenv("MERC_CONNECT_RETURN_URL")) != "" ||
		strings.TrimSpace(os.Getenv("MERC_CONNECT_REFRESH_URL")) != ""
	stagingWithoutConnect := strings.EqualFold(strings.TrimSpace(os.Getenv("MERC_ENV")), "staging") &&
		!connectConfigured
	if stagingWithoutConnect {
		if billingWebhookSecret != "" && !strings.HasPrefix(billingWebhookSecret, "whsec_") {
			return fmt.Errorf("private canary billing webhook must be a whsec_* secret when set")
		}
		if connectWebhookSecret != "" && !strings.HasPrefix(connectWebhookSecret, "whsec_") {
			return fmt.Errorf("private canary Connect webhook must be a whsec_* secret when set")
		}
		// Not requiring a ca_* platform ID here is the whole point of the
		// exception. Not requiring DISTINCT secrets is a different claim, and it
		// is false: /v1/stripe/connect-webhook is served whether or not Connect
		// is onboarded, so equal secrets make a cash-signed event acceptable at
		// the Connect authority and vice versa. That is the exact cross-authority
		// forgery the security suite proves is refused, so the boot check must
		// not be the thing that permits it. Caught on the live staging plane,
		// where both variables had in fact been set to the same whsec_.
		if billingWebhookSecret != "" && billingWebhookSecret == connectWebhookSecret {
			return fmt.Errorf("private canary requires distinct cash and Connect whsec_* endpoint secrets")
		}
		return nil
	}
	if !strings.HasPrefix(billingWebhookSecret, "whsec_") ||
		!strings.HasPrefix(connectWebhookSecret, "whsec_") ||
		billingWebhookSecret == connectWebhookSecret {
		return fmt.Errorf("private canary requires distinct cash and Connect whsec_* endpoint secrets")
	}
	if !strings.HasPrefix(connectClientID, "ca_") {
		return fmt.Errorf("private canary requires a test-mode MERC_CONNECT_CLIENT_ID")
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("control: ")

	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(300 << 20) // 300 MiB
	}

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	// Gateway parity live driver — same EvaluateGatewayParityGate /
	// BuildGatewayParityReceipt path the unit tests exercise. Thin scripts
	// re-exec this command; do not reimplement the gate in ops/scripts/.
	if len(os.Args) > 1 && os.Args[1] == "gateway-parity" {
		os.Exit(runGatewayParityCLI(os.Args[2:]))
	}

	if len(os.Args) > 1 && os.Args[1] == "print-claim-sql" {
		predicate := "t.claimed_by IS NULL"
		if len(os.Args) > 2 && os.Args[2] == "--pinned" {
			predicate = "t.claimed_by = $1 AND t.started_at IS NULL"
		}
		fmt.Print(ClaimTaskSQL(predicate))
		return
	}

	if len(os.Args) > 1 && os.Args[1] != "serve" {
		if dispatchDev(os.Args[1], os.Args[2:]) {
			return
		}
		if dispatchRelease(os.Args[1], os.Args[2:]) {
			return
		}
		if dispatchBuyer(os.Args[1], os.Args[2:]) {
			return
		}
		if dispatchProject(os.Args[1], os.Args[2:]) {
			return
		}
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set  -  refusing to start (set it to a Postgres connection string)")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("CONTROL_PLANE_ADDR")
	}
	if addr == "" {
		addr = ":8080"
	}

	missingHardeningSecret, hardeningErr := validateHardeningSecretConfig(
		os.Getenv("MERC_ENV"), stripeKey(), os.Getenv("MERC_TOKEN_KEY"),
		os.Getenv("MERC_VERIFICATION_SAMPLE_SECRET"),
	)
	if hardeningErr != nil {
		log.Fatal(hardeningErr)
	}
	if missingHardeningSecret {
		log.Print("WARNING: MERC_TOKEN_KEY and/or MERC_VERIFICATION_SAMPLE_SECRET missing or unsafe  -  OAuth token/webhook-secret storage or verification sampling is unhardened; set both to at least 32 unpredictable bytes before production")
	}
	if err := validateAdminAccessConfig(os.Getenv("MERC_ENV"), os.Getenv("MERC_ADMIN_CIDRS")); err != nil {
		log.Fatal(err)
	}
	settlementCur, err := LoadSettlementCurrencyFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("settlement currency: %s (exponent=%d)", settlementCur.Code(), settlementCur.Exponent())
	paymentAuthority, err := currentPaymentAuthority()
	if err != nil {
		log.Fatal(err)
	}
	stripeSecret, err := configuredStripeKey()
	if err != nil {
		log.Fatal(err)
	}
	if err := validatePaymentAuthorityStartup(
		paymentAuthority, os.Getenv("MERC_PAYMENT_PROVIDER"), os.Getenv("STRIPE_WEBHOOK_SECRET"),
		os.Getenv("MERC_CONNECT_WEBHOOK_SECRET"), os.Getenv("MERC_CONNECT_CLIENT_ID"),
		os.Getenv("MERC_PAYOUT_EXPORT"),
	); err != nil {
		log.Fatal(err)
	}
	if paymentAuthority.Mode == PaymentModeLive {
		if _, err := LoadEconomicScheduleFromEnv(); err != nil {
			log.Fatalf("LIVE payment configuration: economic schedule: %v", err)
		}
		if err := validateConnectURLPair(
			os.Getenv("MERC_CONNECT_RETURN_URL"), os.Getenv("MERC_CONNECT_REFRESH_URL"),
			os.Getenv("SITE_HOST"),
		); err != nil {
			log.Fatalf("LIVE Stripe Connect configuration invalid: %v", err)
		}
	}
	log.Printf("payment authority: mode=%s provider_enabled=%t live_value_movement=%t stripe_api_version=%s",
		paymentAuthority.Mode, paymentAuthority.ProviderEnabled(),
		paymentAuthority.LiveValueMovementEnabled(), stripeAPIVersion)
	if err := validateCanaryMoneyMode(
		os.Getenv("MERC_CANARY_MODE"), stripeKey(), os.Getenv("STRIPE_WEBHOOK_SECRET"),
		os.Getenv("MERC_CONNECT_WEBHOOK_SECRET"), os.Getenv("MERC_CONNECT_CLIENT_ID"),
		os.Getenv("MERC_PAYOUT_EXPORT"),
	); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatalf("invalid DATABASE_URL: %v", err)
	}
	poolCfg.MaxConns, err = parseDBMaxConns(os.Getenv("DB_MAX_CONNS"))
	if err != nil {
		log.Fatalf("invalid database pool configuration: %v", err)
	}
	seedMode := len(os.Args) > 1 && os.Args[1] == "seed"
	runWorkers := os.Getenv("MERC_RUN_WORKERS") != "false"
	if !seedMode {
		if err := validateExecutionDBCapacity(poolCfg.MaxConns, runWorkers); err != nil {
			log.Fatalf("invalid database pool configuration: %v", err)
		}
	}
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatalf("failed to create pgx pool: %v", err)
	}
	defer pool.Close()
	log.Printf("db pool: max_conns=%d conn_max_lifetime=%s conn_max_idle=%s", poolCfg.MaxConns, poolCfg.MaxConnLifetime, poolCfg.MaxConnIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatalf("database unreachable at startup: %v", err)
	}

	store := NewStore(pool)
	if runWorkers && !seedMode {
		store = NewStoreWithWorkerLeader(pool)
	}

	if seedMode {
		if err := validateSeedAllowed(os.Getenv("MERC_ENV"), stripeKey()); err != nil {
			log.Fatal(err)
		}
		if err := store.Migrate(ctx); err != nil {
			log.Fatalf("migrate before seed failed: %v", err)
		}
		seedStorage, serr := NewStorage(ctx)
		if serr != nil {
			log.Printf("seed: object storage unavailable (%v)  -  honeypot input object will NOT be uploaded; DB-only seed", serr)
			seedStorage = nil
		}
		if err := seedDemo(ctx, pool, seedStorage); err != nil {
			log.Fatalf("seed failed: %v", err)
		}
		return
	}

	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate failed: %v", err)
	}

	priceSchedule, rerr := BuildCataloguePriceSchedule()
	if rerr != nil {
		log.Fatalf("catalogue price authority unavailable: %v", rerr)
	}
	if n, rerr := store.ApplyRepricing(ctx, priceSchedule); rerr != nil {
		log.Fatalf("apply catalogue price schedule: %v", rerr)
	} else if n > 0 {
		log.Printf("catalogue price schedule %s: %d model price(s) updated atomically",
			priceSchedule.SHA256, n)
	}
	// SupplierViabilityReport computes, per catalogue model, whether a supplier
	// clears their own electricity at the price we advertise. It existed but was
	// only ever called from a test, so an underwater supply side was discoverable
	// only by a supplier reading their power bill. Say it out loud at boot.
	for _, v := range SupplierViabilityReport() {
		if v.Viable {
			continue
		}
		log.Printf("WARNING: supplier economics underwater: model=%s job=%s hw=%s "+
			"gross=$%.6f/hr electricity=$%.6f/hr net=$%.6f/hr; "+
			"break-even needs %.1f units/s, measured %.1f",
			v.ModelID, v.JobType, v.HWClass, v.SupplierGrossUSDHr, v.ElectricityUSDHr,
			v.NetUSDHr, v.BreakEvenUnitsPerSec, v.MeasuredUnitsPerSec)
	}

	if err := store.ValidateAdvertisedRuntimeCatalog(ctx); err != nil {
		log.Fatalf("runtime catalog validation failed: %v", err)
	}

	storage, err := NewStorage(ctx)
	if err != nil {
		log.Fatalf("object storage init failed: %v", err)
	}

	var payout Payout = stubPayout{}
	if paymentAuthority.ProviderEnabled() {
		sp := newStripePayout(store, stripeSecret)
		payout = sp
		log.Printf("payout rail: Stripe Connect (%s authority)", paymentAuthority.Mode)

		// The ledger settles in MERC_SETTLEMENT_CURRENCY. A platform that
		// cannot hold that currency accepts every transfer request and fails
		// it at the moment money moves, so refuse here rather than discovering
		// it per payout.
		probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
		err := sp.verifySettlementCurrency(probeCtx)
		if err != nil {
			where := sp.accountCountry(probeCtx)
			if where != "" {
				where = " [account " + where + "]"
			}
			if paymentAuthority.LiveValueMovementEnabled() {
				cancelProbe()
				log.Fatalf("stripe settlement preflight failed%s: %v", where, err)
			}
			log.Printf("WARNING: stripe settlement preflight failed%s: %v", where, err)
		}
		cancelProbe()
	} else {
		log.Print("payout rail: SEALED  -  provider calls, exports, charges, refunds, reversals and payouts are structurally disabled")
	}

	server := NewServer(store, storage, NewVerifier(store).WithStorage(storage), payout)
	// Say which refusal it was, once, where only the operator can read it.
	// /readyz publishes the machine-readable reason code and nothing more: the
	// refusal names the decision artifact's path and digest, and an unauthenticated
	// probe is not the place to hand those out. Without this line the operator's
	// only evidence would be a 503 with no cause.
	if server.canary.Enabled && server.canary.configError != nil {
		log.Printf("canary policy is incomplete, buyer admission stays closed: %v", server.canary.configError)
	}

	workersCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workers := NewWorkers(store, storage, payout)
	if runWorkers {
		setWorkerElectionReadinessEnabled(true)
		go runWorkerLeader(workersCtx, pool, workers)
	} else {
		setWorkerElectionReadinessEnabled(false)
		log.Print("MERC_RUN_WORKERS=false · background workers explicitly disabled on this instance")
	}
	go server.startRateLimitSweeper(workersCtx) // evict idle rate-limit buckets
	go startTaskWakeListener(workersCtx, pool)

	srv := newHTTPServer(addr, server.Routes())

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("shutdown signal received, draining...")

	stopWorkers() // stop the background tickers first

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	// Drain observational admission telemetry after HTTP has stopped accepting
	// so no new enqueues race the close. Bound: queueCap on hard kill only.
	server.CloseAdmissionTelemetry(5 * time.Second)
	log.Print("stopped")
}

func runHealthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("CONTROL_PLANE_ADDR")
	}
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else {
		addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		log.Printf("healthcheck: %v", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("healthcheck: status %d", resp.StatusCode)
		return 1
	}
	return 0
}
