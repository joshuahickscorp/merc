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

// main.go — process entrypoint: read config from the environment, open the
// Postgres pool (FATAL if DATABASE_URL is unset — no silent fallback), apply the
// control-plane migrations, build the object store, wire the single
// http.ServeMux, start the background workers, and run with graceful shutdown.
//
// The whole control plane is one binary, one flat package main. The Postgres-
// backed task queue (scheduler.go) means there is no NATS, no message broker,
// and no second dev service to run — just this binary, a Postgres, and an
// S3-compatible object store.
//
// `control seed` runs the idempotent demo seed (seed.go) and exits without
// starting the server, so a human can obtain an api_key + worker_token to drive
// the system end to end.

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
			"CX_TOKEN_KEY and/or CX_VERIFICATION_SAMPLE_SECRET missing or unsafe with %s — refusing to start: OAuth token storage, webhook signing-secret storage, or verification sampling would be unhardened; set both to at least 32 unpredictable bytes",
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

// validateLiveMoneyConfig fails closed before the process opens a database or
// starts serving when the deployment itself says real money is in scope. Quote
// and submission handlers also validate the economic schedule, but waiting for
// the first customer request would let /readyz stay green on a deployment that
// cannot quote, save a card, or learn that a supplier completed Connect KYC.
//
// Stripe signs each separately registered webhook endpoint with its own secret.
// Reusing one value for both routes is therefore rejected as configuration drift,
// not treated as a convenient fallback. Error text names variables only and never
// includes secret material.
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

	// E1 · soft-memory valve. Give the GC a hard ceiling so the control plane GCs
	// harder as it approaches the limit instead of growing until the container's
	// cgroup OOM-kills it (a hard kill drops in-flight requests; a soft limit just
	// makes the GC work harder). Honour an operator-set GOMEMLIMIT verbatim; only
	// when unset do we apply a conservative ~300MiB default that sits below the
	// docker-compose mem_limit so the runtime reacts before the cgroup does.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(300 << 20) // 300 MiB
	}

	// `control healthcheck`: in-container readiness probe against the running server's
	// /readyz — the distroless image has no shell/curl for a Docker HEALTHCHECK. It
	// asks the already-running server, including DB + owned-sweep liveness.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	// `control print-claim-sql`: print the EXACT, verbatim SQL text ClaimTask
	// (scheduler.go) executes for the claim CTE — no DB, no server, just the
	// rendered string from ClaimTaskSQL to stdout.
	//
	// PATCH (Control plane hot path 4.5->5, docs/internal/CREED_AND_PATH_TO_TEN.md
	// "Make the benchmark measure the real query"): this is the seam
	// scripts/bench-local.sh uses to EXPLAIN ANALYZE the literal production
	// query instead of a hand-copied stand-in. Because it calls ClaimTaskSQL —
	// the SAME function ClaimTask itself calls — there is no second copy of
	// this SQL anywhere to drift out of sync; a change to the real query is a
	// change to what this prints, by construction, not by discipline. Default
	// prints the general "claimed_by IS NULL" branch — the common,
	// index-servable path tasks_ready_unclaimed_idx is meant to serve, and the
	// one the benchmark should measure; `--pinned` prints the rare
	// pinned-branch predicate instead.
	if len(os.Args) > 1 && os.Args[1] == "print-claim-sql" {
		predicate := "t.claimed_by IS NULL"
		if len(os.Args) > 2 && os.Args[2] == "--pinned" {
			predicate = "t.claimed_by = $1 AND t.started_at IS NULL"
		}
		fmt.Print(ClaimTaskSQL(predicate))
		return
	}

	// Buyer/operator + evidence subcommands of the one cx binary (submit, quote,
	// status, ..., version, audit, source-id, verify) are pure HTTP clients or
	// local git tools. They must run WITHOUT DATABASE_URL and must NOT boot the
	// control plane, so dispatch them HERE, before the mandatory DB gate below.
	// No-arg and `serve` (and any unrecognized arg) fall through to the server.
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		if dispatchBuyer(os.Args[1], os.Args[2:]) {
			return
		}
	}

	// DATABASE_URL is mandatory. BLACKHOLE doctrine: surface every failure —
	// a missing DSN is a fatal misconfiguration, never a fallback to nothing.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set — refusing to start (set it to a Postgres connection string)")
	}
	// LISTEN_ADDR (CONTROL_PLANE_ADDR is the .env alias) sets the bind address.
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("CONTROL_PLANE_ADDR")
	}
	if addr == "" {
		addr = ":8080"
	}

	// Hardening secrets: without these, OAuth tokens are stored unencrypted,
	// customer webhook registration cannot safely persist its independent signing
	// key, or verification sampling is predictable. OAuth state is independently
	// random, hash-at-rest, single-use, expiry-bound, and cookie-bound, with no reusable
	// environment secret. In production, either missing/unsafe hardening key is FATAL —
	// a prod deploy must never silently run unhardened. Outside
	// production (CX_ENV unset or any other value) local dev + tests run without them
	// by design, so we only surface the insecure state loudly. Production sets both
	// in .env.
	//
	// Security Posture 6.5->7 (docs/internal/CREED_AND_PATH_TO_TEN.md): CX_ENV requires
	// the operator to have ALSO remembered to set that separate flag correctly — a real
	// deploy could carry a LIVE Stripe key (real money moving) while CX_ENV was simply
	// never set, and this gate would only WARN. A live Stripe key is a harder-to-miss,
	// self-evident signal that this is production: if STRIPE_SECRET_KEY starts with
	// "sk_live_", real money is on the line regardless of what CX_ENV says, so the same
	// FATAL gate applies — an operator cannot accidentally ship live payments unhardened
	// just by forgetting one env var when a different one already says the stakes.
	missingHardeningSecret, hardeningErr := validateHardeningSecretConfig(
		os.Getenv("CX_ENV"), stripeKey(), os.Getenv("CX_TOKEN_KEY"),
		os.Getenv("CX_VERIFICATION_SAMPLE_SECRET"),
	)
	if hardeningErr != nil {
		log.Fatal(hardeningErr)
	}
	if missingHardeningSecret {
		log.Print("WARNING: CX_TOKEN_KEY and/or CX_VERIFICATION_SAMPLE_SECRET missing or unsafe — OAuth token/webhook-secret storage or verification sampling is unhardened; set both to at least 32 unpredictable bytes before production")
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

	// Open the pool with bounded sizing so a request burst can't exhaust Postgres
	// connections, and idle/aged conns are recycled. MaxConns overridable via
	// DB_MAX_CONNS. We ping once at startup so a bad DSN fails fast and loud.
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

	// `control seed`: run the demo seed and exit (no server required — object
	// store IS attempted, best-effort, so the seeded honeypot's input object
	// actually exists for a real worker to fetch; see seed.go's seedDemo doc
	// comment). Stable demo UUIDs + ON CONFLICT DO NOTHING make it re-runnable.
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		if err := validateSeedAllowed(os.Getenv("CX_ENV"), stripeKey()); err != nil {
			log.Fatal(err)
		}
		seedStorage, serr := NewStorage(ctx)
		if serr != nil {
			log.Printf("seed: object storage unavailable (%v) — honeypot input object will NOT be uploaded; DB-only seed", serr)
			seedStorage = nil
		}
		if err := seedDemo(ctx, pool, seedStorage); err != nil {
			log.Fatalf("seed failed: %v", err)
		}
		return
	}

	// Apply the control-plane schema additions (idempotent). Fatal on error —
	// a half-migrated DB is never a silent fallback.
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate failed: %v", err)
	}

	// Buyer Advantage & Pricing Edge 4.5->5 (docs/internal/CREED_AND_PATH_TO_TEN.md,
	// "Reprice from real supplier economics, not hand-seeded constants"): replace
	// the hand-seeded catalogue prices (still price_source='seed' — never an
	// operator's own edit, see ApplyRepricing) with prices derived from the real
	// measured throughput docs/GPU_CAPABILITY.md publishes and the real supplier
	// share rate. Best-effort at startup (never fatal — a pricing update is not a
	// reason to refuse to serve).
	if n, rerr := store.ApplyRepricing(ctx, RepriceCatalogueFromSupplierEconomics(supplierShareRate)); rerr != nil {
		log.Printf("repricing from supplier economics: %v (catalogue left as-is)", rerr)
	} else if n > 0 {
		log.Printf("repricing from supplier economics: %d catalogue price(s) updated from measured throughput", n)
	}
	// A generated production runtime cell is a promise that its model is priced,
	// resolvable, and memory-gated in the live DB catalog. Refuse to start when the
	// duplicated schema/seed rows drift below that promise; pending catalog rows are
	// allowed but never become production authority merely by existing.
	if err := store.ValidateAdvertisedRuntimeCatalog(ctx); err != nil {
		log.Fatalf("runtime catalog validation failed: %v", err)
	}

	// Object storage is mandatory: a missing/unreachable store is fatal here, not
	// a silent degradation of the job lifecycle.
	storage, err := NewStorage(ctx)
	if err != nil {
		log.Fatalf("object storage init failed: %v", err)
	}

	// Payout rail: real Stripe Connect (STRIPE_SECRET_KEY), else the alpha manual
	// export (CX_PAYOUT_EXPORT — owed credits appended to a CSV for out-of-band
	// settlement), else the honest stub (funded credits reach 'ready'/owed, never
	// 'released' without a real transfer). One selection, shared by the server and
	// the background release worker.
	var payout Payout = stubPayout{}
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		payout = newStripePayout(store, key)
		log.Print("payout rail: Stripe Connect (STRIPE_SECRET_KEY set)")
	} else if path := os.Getenv("CX_PAYOUT_EXPORT"); path != "" {
		payout = newManualExportPayout(path)
		log.Printf("payout rail: manual export (alpha) → %s — owed credits appended for out-of-band settlement", path)
	} else {
		log.Print("payout rail: none configured — funded credits reach 'ready' (owed), never 'released'")
	}

	server := NewServer(store, storage, NewVerifier(store).WithStorage(storage), payout)

	// Background workers (payout release, stale-task requeue, webhook delivery /
	// job sweep) run for the life of the process, bound to a context cancelled on
	// shutdown so the tickers stop cleanly.
	workersCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workers := NewWorkers(store, storage, payout)
	// Every eligible replica enters PostgreSQL-backed leader election. Exactly one
	// holds the session lock and runs side-effecting sweeps; standbys keep serving API
	// traffic and take over automatically when that session/process fails. The env
	// switch remains an explicit maintenance escape hatch, not the HA mechanism.
	if os.Getenv("CX_RUN_WORKERS") != "false" {
		setWorkerElectionReadinessEnabled(true)
		go runWorkerLeader(workersCtx, pool, workers)
	} else {
		setWorkerElectionReadinessEnabled(false)
		log.Print("CX_RUN_WORKERS=false · background workers explicitly disabled on this instance")
	}
	go server.startRateLimitSweeper(workersCtx) // evict idle rate-limit buckets
	// Wake-on-work (notify.go): runs on EVERY instance regardless of CX_RUN_WORKERS —
	// unlike the sweep workers above, claim long-polls are served by every instance,
	// not just the primary, so every instance needs its own LISTEN connection.
	go startTaskWakeListener(workersCtx, pool)

	srv := newHTTPServer(addr, server.Routes())

	// Run the server until a signal arrives.
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
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

// runHealthcheck probes the running control plane's /readyz over loopback and
// returns a process exit code (0 ready, 1 not). It backs the distroless image's
// Docker HEALTHCHECK (no shell/curl available there).
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
