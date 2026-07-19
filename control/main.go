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
	verificationSecretUnsafe := len(verificationSampleSecret) < 32 ||
		verificationSampleSecret == insecureDevelopmentSamplingSecret
	missing := tokenKeyUnsafe || verificationSecretUnsafe
	if !missing {
		return false, nil
	}
	liveStripe := strings.HasPrefix(stripeSecret, "sk_live_")
	if strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod") || liveStripe {
		reason := "CX_ENV=" + cxEnv
		if liveStripe {
			reason = "STRIPE_SECRET_KEY is a LIVE key (sk_live_...)"
		}
		return true, fmt.Errorf(
			"CX_TOKEN_KEY and/or CX_VERIFICATION_SAMPLE_SECRET missing or unsafe with %s  -  refusing to start: OAuth token storage, webhook signing-secret storage, or verification sampling would be unhardened; set both to at least 32 unpredictable bytes",
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

func validateSeedAllowed(cxEnv, stripeSecret string) error {
	production := strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod")
	if production || strings.HasPrefix(stripeSecret, "sk_live_") {
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

func validateLiveMoneyConfig(cxEnv, stripeSecret, billingWebhookSecret, connectWebhookSecret string) error {
	liveStripe := strings.HasPrefix(stripeSecret, "sk_live_")
	production := strings.EqualFold(cxEnv, "production") || strings.EqualFold(cxEnv, "prod")
	if !production && !liveStripe {
		return nil
	}

	missing := make([]string, 0, 3)
	if production && strings.TrimSpace(stripeSecret) == "" {
		missing = append(missing, "STRIPE_SECRET_KEY")
	}
	if strings.TrimSpace(billingWebhookSecret) == "" {
		missing = append(missing, "STRIPE_WEBHOOK_SECRET")
	}
	if strings.TrimSpace(connectWebhookSecret) == "" {
		missing = append(missing, "CX_CONNECT_WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("live money configuration invalid: %s required; refusing to start", strings.Join(missing, ", "))
	}
	if billingWebhookSecret == connectWebhookSecret {
		return fmt.Errorf("live money configuration invalid: STRIPE_WEBHOOK_SECRET and CX_CONNECT_WEBHOOK_SECRET must be distinct endpoint secrets; refusing to start")
	}
	if _, err := LoadEconomicScheduleFromEnv(); err != nil {
		return fmt.Errorf("live money configuration invalid: economic schedule: %w; refusing to start", err)
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

	if len(os.Args) > 1 && os.Args[1] == "print-claim-sql" {
		predicate := "t.claimed_by IS NULL"
		if len(os.Args) > 2 && os.Args[2] == "--pinned" {
			predicate = "t.claimed_by = $1 AND t.started_at IS NULL"
		}
		fmt.Print(ClaimTaskSQL(predicate))
		return
	}

	if len(os.Args) > 1 && os.Args[1] != "serve" {
		if dispatchBuyer(os.Args[1], os.Args[2:]) {
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
		os.Getenv("CX_ENV"), stripeKey(), os.Getenv("CX_TOKEN_KEY"),
		os.Getenv("CX_VERIFICATION_SAMPLE_SECRET"),
	)
	if hardeningErr != nil {
		log.Fatal(hardeningErr)
	}
	if missingHardeningSecret {
		log.Print("WARNING: CX_TOKEN_KEY and/or CX_VERIFICATION_SAMPLE_SECRET missing or unsafe  -  OAuth token/webhook-secret storage or verification sampling is unhardened; set both to at least 32 unpredictable bytes before production")
	}
	if err := validateLiveMoneyConfig(
		os.Getenv("CX_ENV"), stripeKey(), os.Getenv("STRIPE_WEBHOOK_SECRET"),
		os.Getenv("CX_CONNECT_WEBHOOK_SECRET"),
	); err != nil {
		log.Fatal(err)
	}
	if err := validateLiveConnectURLConfig(
		os.Getenv("CX_ENV"), stripeKey(), os.Getenv("CX_CONNECT_RETURN_URL"),
		os.Getenv("CX_CONNECT_REFRESH_URL"), os.Getenv("SITE_HOST"),
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

	if len(os.Args) > 1 && os.Args[1] == "seed" {
		if err := validateSeedAllowed(os.Getenv("CX_ENV"), stripeKey()); err != nil {
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

	if n, rerr := store.ApplyRepricing(ctx, RepriceCatalogueFromSupplierEconomics(supplierShareRate)); rerr != nil {
		log.Printf("repricing from supplier economics: %v (catalogue left as-is)", rerr)
	} else if n > 0 {
		log.Printf("repricing from supplier economics: %d catalogue price(s) updated from measured throughput", n)
	}
	if err := store.ValidateAdvertisedRuntimeCatalog(ctx); err != nil {
		log.Fatalf("runtime catalog validation failed: %v", err)
	}

	storage, err := NewStorage(ctx)
	if err != nil {
		log.Fatalf("object storage init failed: %v", err)
	}

	var payout Payout = stubPayout{}
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		payout = newStripePayout(store, key)
		log.Print("payout rail: Stripe Connect (STRIPE_SECRET_KEY set)")
	} else if path := os.Getenv("CX_PAYOUT_EXPORT"); path != "" {
		payout = newManualExportPayout(path)
		log.Printf("payout rail: manual export (alpha) -> %s  -  owed credits appended for out-of-band settlement", path)
	} else {
		log.Print("payout rail: none configured  -  funded credits reach 'ready' (owed), never 'released'")
	}

	server := NewServer(store, storage, NewVerifier(store).WithStorage(storage), payout)

	workersCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workers := NewWorkers(store, storage, payout)
	if os.Getenv("CX_RUN_WORKERS") != "false" {
		setWorkerElectionReadinessEnabled(true)
		go runWorkerLeader(workersCtx, pool, workers)
	} else {
		setWorkerElectionReadinessEnabled(false)
		log.Print("CX_RUN_WORKERS=false · background workers explicitly disabled on this instance")
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
