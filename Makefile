# Computexchange — dev tasks. Targets are honest: if a tool is missing they
# fail loudly, never pass silently. `.env` is loaded if present so DATABASE_URL
# (and friends) flow into migrate/control/prove-local without re-typing.
#
# The headline target is `prove-local`: ONE command that boots a throwaway stack
# and runs the named source-bound local contract matrix (see scripts/prove-local.sh
# + proof/5x5-gates.json). Passing it is not whole-product or launch proof.

ifneq (,$(wildcard .env))
include .env
export
endif

DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

.PHONY: up down dev-up dev-down migrate seed control agent-run agent-bench \
        prove-local bench-local metrics build fmt test spec-test loc audit docker-build \
        install uninstall backup

# ── Full stack (containerized control plane + deps) ──────────────────────────
# Brings up Postgres, MinIO, the bucket+schema one-shots, and the Go control
# plane (built from Dockerfile.control), in dependency order. The control plane
# is reachable on :8080. The Rust agent is NOT containerized (needs Metal) — run
# it natively with `make agent-run` pointed at CX_CONTROL_URL=http://localhost:8080.
up:
	@command -v docker >/dev/null || { echo "ERROR: docker not found (install Docker Desktop)"; exit 1; }
	docker compose up -d --build

down:
	docker compose down

# ── Dev stack (just the deps; run control/agent natively) ────────────────────
# Postgres + MinIO + the createbuckets one-shot. Schema is applied separately
# via `make migrate`. Use this when iterating on the Go control plane locally.
dev-up:
	@command -v docker >/dev/null || { echo "ERROR: docker not found (install Docker Desktop)"; exit 1; }
	docker compose up -d postgres minio createbuckets

dev-down:
	docker compose down

# Apply the single authoritative schema. Idempotent (schema.sql uses IF NOT EXISTS).
migrate:
	@command -v psql >/dev/null || { echo "ERROR: psql not found (install the postgresql client)"; exit 1; }
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 --single-transaction -f db/schema.sql

# Mint a demo buyer api_key + worker_token and print them (idempotent). Inserts a
# hashed api_key into api_keys and a token into worker_tokens, then exits without
# starting the server. Copy the printed worker_token into .env (CX_WORKER_TOKEN).
seed:
	cd control && go run . seed

# Run the Go control plane (foreground, :8080).
control:
	cd control && go run .

# Run the Rust supplier agent (polling loop). Needs agent/agent.toml (gitignored,
# holds the worker token); CX_CONTROL_URL + CX_WORKER_TOKEN env override its fields.
agent-run:
	@test -f agent/agent.toml || { echo "ERROR: agent/agent.toml missing — run: cp agent/agent.example.toml agent/agent.toml  (then set worker_token from 'make seed', or export CX_WORKER_TOKEN)"; exit 1; }
	cd agent && cargo run --release -- run

# Run the Rust agent's benchmark subcommand (no Python bench/ — folded in here).
agent-bench:
	@test -f agent/agent.toml || { echo "ERROR: agent/agent.toml missing — run: cp agent/agent.example.toml agent/agent.toml"; exit 1; }
	cd agent && cargo run --release -- bench

# THE local proof: provisions a throwaway native Postgres + MinIO (no Docker
# pulls; USE_DOCKER=1 to switch), applies the schema, runs the deterministic
# matrix (go test -tags integration: auth, verification, honeypot/fraud,
# idempotency, requeue, payout hold→ready, webhooks, malformed input, metrics),
# then drives the REAL Rust agent through LIVE Metal inference (embed + infer +
# whisper) and prints a PROOF LEDGER. Fails loudly; exit non-zero on any gap.
#   SKIP_LIVE=1 (matrix only) · KEEP=1 (leave stack up) · PROVE_WHISPER=1 (require whisper)
prove-local:
	bash scripts/prove-local.sh

# OPTIONAL local benchmark lab (Plane D §16 D10) — SEPARATE from prove-local
# (correctness). Provisions a throwaway native Postgres + MinIO the same way, then
# measures p50/p90 for (a) POST /v1/quote latency, (b) the ready-task claim query
# (EXPLAIN ANALYZE over a synthetic 1k queue), and (c) embed JSON-vs-binary artifact
# size, writing .artifacts/bench-local/report.md (UTC timestamp + git commit + machine
# class). Needs no external services and never alters prove-local.
#   KEEP=1 (reuse+leave stack up) · QUOTE_CALLS / CLAIM_RUNS / SYNTH_TASKS / EMBED_ROWS
bench-local:
	bash scripts/bench-local.sh

# Scrape the Prometheus metrics endpoint (control plane must be running).
metrics:
	@command -v curl >/dev/null || { echo "ERROR: curl not found"; exit 1; }
	curl -s localhost:8080/metrics

# Compile both sides. Fails if either toolchain is absent.
build:
	cd agent && cargo build
	cd spec-engine && cargo build --all-targets
	cd token-spec-poc && cargo build --all-targets
	cd control && go build ./...

# Format both sides in place.
fmt:
	cd agent && cargo fmt
	cd control && gofmt -w .

# Test both sides.
test:
	cd agent && cargo test
	cd spec-engine && cargo test --all-targets
	cd token-spec-poc && cargo test --all-targets
	python3 scripts/spec-lab/test_cx_speculative_core.py
	python3 scripts/spec-lab/test_cx_render_spec_adapter.py
	python3 scripts/spec-lab/test_cx_transcode_spec_adapter.py
	python3 scripts/spec-lab/test_cx_integrated_speculation.py
	cd control && go test ./...

# Focused, cloud-free speculative substrate gate (same core as CI).
spec-test:
	cd spec-engine && cargo test --all-targets
	cd token-spec-poc && cargo test --all-targets
	python3 scripts/spec-lab/test_cx_speculative_core.py
	python3 scripts/spec-lab/test_cx_render_spec_adapter.py
	python3 scripts/spec-lab/test_cx_transcode_spec_adapter.py
	python3 scripts/spec-lab/test_cx_integrated_speculation.py
	python3 scripts/spec-lab/test_cx_native_speculation_ladder.py
	python3 scripts/spec-lab/emit_current_spec_receipts.py > /tmp/cx-python-spec-receipts.jsonl
	cargo run --quiet --manifest-path spec-engine/Cargo.toml --example validate_receipts < /tmp/cx-python-spec-receipts.jsonl
	cd token-spec-poc && cargo run --quiet > /tmp/cx-token-spec-receipts.jsonl
	sed -n '1p' /tmp/cx-token-spec-receipts.jsonl > /tmp/cx-token-spec-first.jsonl
	cargo run --quiet --manifest-path spec-engine/Cargo.toml --example validate_receipts < /tmp/cx-token-spec-first.jsonl

# Authoritative codebase census — RETIRES the old selective `find | wc -l`.
# `cx audit codebase` walks the whole git-tracked set and classifies every file
# by language, subsystem, layer, ship/generated/vendored/pack status, writing the
# census/ artifacts (CODEBASE_CENSUS.{json,md}, CODEBASE_LOC.json, CODEBASE_BYTES.json,
# PYTHON_RECLAMATION.json, DEPENDENCY_CENSUS.json, CONDENSATION_CANDIDATES.json).
# Deterministic for a fixed commit. This is the headline measurement now.
loc: audit
audit:
	cd control && go run . audit codebase --out census
	@echo "— census/ written (relative to repo root). Headline numbers:"
	@sed -n '3p' census/CODEBASE_CENSUS.md

# Build the control-plane image standalone (CI / registry push).
docker-build:
	@command -v docker >/dev/null || { echo "ERROR: docker not found"; exit 1; }
	docker build -f Dockerfile.control -t cx-control .

# ── Supplier-agent install / operations (macOS) ──────────────────────────────
# One-command install (build from source + LaunchAgent) and clean uninstall. The
# signed, notarized .app is the external step (Apple Developer ID); these are the
# local build-from-source path. `make install ARGS=--check` for a dry run.
install:
	bash scripts/install.sh $(ARGS)

uninstall:
	bash scripts/uninstall.sh $(ARGS)

# Timestamped backup of the ledger/jobs DB + local object store (docs/RUNBOOKS.md).
# The `disaster-recovery` check in prove-local proves the dump is restorable.
backup:
	bash scripts/backup.sh

# Render the two site oracles (Mac Studio + DGX Spark) in the product's Cycles rig,
# then generate the 1x/2x srcset downsizes and the og:image composite. Headless
# Metal GPU; previews and the continuity sheet live in render/site/previews/.
BLENDER ?= /Applications/Blender.app/Contents/MacOS/Blender
render-oracles:
	@[ -x "$(BLENDER)" ] || { echo "ERROR: Blender not found at $(BLENDER) (set BLENDER=)"; exit 1; }
	"$(BLENDER)" -b -P render/site/oracles.py -- --out web/assets/site/ --samples 2048
	bash render/site/downsize.sh
