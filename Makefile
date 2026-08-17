DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

.PHONY: credentials credentials-check droplet-deploy private-canary realtime-sdk-conformance up down dev-up dev-down test-unit license-register release-gates alert-delivery-test backup-age-metric-test migrate seed control agent-run agent-bench agent-characterize prove-local metrics build fmt test test-integration ci audit loc docker-build install uninstall backup restore-drill backup-envelope-test local-independent-restore offsite-independent-restore offsite-independent-restore-check local-production-tls local-rollback restart-storm-local technical-exercises recovery-suite alert-check alert-page render-staging validate-staging soak-15m soak-2h soak-24h soak-24h-persistent soak-24h-status release-doctor stripe-simulate stripe-check stripe-matrix secret-audit approvals-check mutation-test mutation-test-parallel mutation-fast mutation-authority mutation-full mutation-deep alpha-security stripe-nonconnect

up:
	docker compose up -d --build

down:
	docker compose down

dev-up:
	docker compose up -d postgres minio createbuckets

dev-down:
	docker compose down

migrate:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql

seed:
	cd control && go run . seed

control:
	cd control && go run .

agent-run:
	cd agent && cargo run --release -- run

agent-bench:
	cd agent && cargo run --release -- bench

agent-characterize:
	cd agent && cargo run --release -- characterize

prove-local:
	cd control && go run . prove --full

metrics:
	curl -fsS localhost:8080/metrics

build:
	cd control && go build ./...
	cd agent && cargo build

fmt:
	cd control && gofmt -w .
	cd agent && cargo fmt --all

# MERC_TEST_DATABASE_URL defaults to the `make dev-up` Compose database so that
# `make ci` runs the integration suite instead of skipping it.  Override it to
# point at another instance; the tests fail closed when it is unreachable.
MERC_TEST_DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

# Object storage and a local inference engine are passed THROUGH when the
# environment provides them, so a developer machine that has `make dev-up` MinIO
# and a llama-server actually runs the artifact, chain and failure-matrix tests
# instead of skipping them. The CI lane has neither and skips them; every one of
# those skips is named in scripts/allowed-test-skips.txt, which is what stops the
# skipping from being invisible.
CI_TEST_ENV = MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" \
  MERC_TEST_S3_ENDPOINT="$(MERC_TEST_S3_ENDPOINT)" \
  MERC_TEST_S3_BUCKET="$(MERC_TEST_S3_BUCKET)" \
  MERC_TEST_S3_ACCESS_KEY="$(MERC_TEST_S3_ACCESS_KEY)" \
  MERC_TEST_S3_SECRET_KEY="$(MERC_TEST_S3_SECRET_KEY)" \
  MERC_LLAMA_EMBED_URL="$(MERC_LLAMA_EMBED_URL)"

# Isolated databases clone a schema-stamped template (CREATE DATABASE TEMPLATE)
# instead of applying control/schema.sql per test. ensure-schema-template.sh
# rebuilds merc_schema_ddl_<sha16> when schema.sql changes; a stale name is refused.
# -parallel 16 matches the mutation-gate worker default.
#
# -timeout 45m, not the 10m default. The suite grew agent-PROCESS tests: two
# merc-agent binaries cold-load and benchmark both retained models before they
# register, so it runs about fourteen minutes on a host that has object storage
# and a local engine. The default killed `make ci` mid-run with a ten-minute
# panic, which read as a hung test rather than as a budget.
#
# `test` is the fast control loop: no agent-subprocess integration tag.
# `test-integration` is TestFirstCompleteLoopThroughThePublicAPI (real merc-agent).
# `ci` runs both tiers together (fast + integration overlapping).
test:
	@template="$$(MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash scripts/ensure-schema-template.sh)"; \
	cd control && $(CI_TEST_ENV) MERC_ISOLATED_TEST_DB_TEMPLATE="$$template" bash ../scripts/with-isolated-test-storage.sh bash ../scripts/with-isolated-test-db.sh go test -timeout 45m -parallel 16 ./...
	cd agent && cargo test
	bash scripts/verify-python-sdk-package.sh

# Agent-subprocess public-API loop. Requires `cargo build --release` in agent/
# so merc-agent exists; object storage comes from with-isolated-test-storage.sh.
test-integration:
	@template="$$(MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash scripts/ensure-schema-template.sh)"; \
	cd control && $(CI_TEST_ENV) MERC_ISOLATED_TEST_DB_TEMPLATE="$$template" bash ../scripts/with-isolated-test-storage.sh bash ../scripts/with-isolated-test-db.sh go test -timeout 45m -tags=integration -parallel 16 -run '^TestFirstCompleteLoopThroughThePublicAPI$$' ./...

# The fast loop: unit tests only, database suite explicitly opted out.  CI never
# sets MERC_ALLOW_SKIPPING_DB_TESTS, so the money and scheduling tests cannot stop
# running there without someone noticing.
test-unit:
	cd control && MERC_ALLOW_SKIPPING_DB_TESTS=1 go test ./...

# The suite is recorded ONCE, as JSON, and both the human summary and the
# skip gate read that record.
#
# assert-no-test-skips.sh used to run the whole suite again: fourteen extra
# minutes on a host with object storage and a local engine, and the second run
# inherited the first one's rows in the shared database, so it failed on tests
# that had just passed. Same 45m budget as `test` above, same reason.
CI_TEST_JSON = $(CURDIR)/.ci-test.json

ci:
	# The public API integration tests launch the release agent as a real
	# subprocess. Build it from this tree before those tests so a stale binary
	# cannot enroll against one capability digest and dispatch another.
	cd agent && cargo build --release
	cd control && test -z "$$(gofmt -l .)" && go vet ./...
	$(CI_TEST_ENV) bash "$(CURDIR)/scripts/run-ci-control-tests.sh" "$(CI_TEST_JSON)"; \
	  status=$$?; python3 "$(CURDIR)/scripts/summarize-go-test-json.py" "$(CI_TEST_JSON)"; \
	  exit $$status
	@bash scripts/assert-no-test-skips.sh "$(CI_TEST_JSON)"
	@bash scripts/test-release-image-boots.sh
	@bash scripts/test-release-image-contents.sh
	cd agent && cargo fmt --all -- --check && cargo clippy --all-targets -- -D warnings && cargo test
	python3 -m json.tool clients/proto/manifest.schema.json >/dev/null
	python3 -m json.tool ops/governance-approval-bundle.schema.json >/dev/null
	python3 -m json.tool ops/live-payment-activation.schema.json >/dev/null
	python3 scripts/runpod-spend-guard.py --self-test
	python3 scripts/test-runpod-orphan-reconcile.py
	python3 scripts/runpod-spend-guard.py revalidate
	python3 scripts/validate-authorization-matrix.py
	python3 scripts/validate-sdk-routes.py
	python3 scripts/validate-independent-reviews.py
	python3 scripts/validate-governance.py
	python3 scripts/validate-readiness.py
	python3 scripts/assert-soak-claims.py
	python3 scripts/validate-claim-surfaces.py
	python3 scripts/rename-residue-audit.py
	bash scripts/test-agent-install-signature.sh
	bash scripts/test-agent-uninstall-legacy.sh
	bash scripts/test-agent-cross-platform-installers.sh
	python3 scripts/validate-repo-boundary.py
	python3 scripts/test-bench-accounting.py
	python3 scripts/test-gateway-parity-receipt.py
	python3 scripts/validate-evidence-binding.py
	python3 scripts/test-evidence-binding-dependencies.py
	python3 scripts/test-evidence-binding-withdrawn.py
	python3 scripts/verify-lfs-corpus.py
	python3 scripts/test-evidence-writer-bypass.py
	bash scripts/test-mutation-test-parallel.sh
	bash scripts/test-mutation-load-preflight.sh
	python3 scripts/test-mutation-test-contracts.py
	python3 scripts/test-mutation-contract-observer.py
	python3 scripts/test-mutation-suite-observer.py
	python3 scripts/test-mutation-preflight-cache.py
	python3 scripts/test-mutation-manifest.py
	bash scripts/test-with-isolated-test-db.sh
	MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash scripts/test-schema-template.sh
	bash scripts/test-mutation-gate.sh
	bash scripts/test-mutation-oracle-strategy.sh
	bash scripts/test-readiness-gaming.sh
	bash scripts/test-agent-review-gaming.sh
	bash scripts/test-technical-exercises-fail-closed.sh
	bash scripts/test-canary-gaming.sh
	bash scripts/test-canary-scenario-receipt.sh
	MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash scripts/test-canary-database-corroboration.sh
	MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash scripts/test-agent-restart-authority.sh
	bash scripts/test-go-closure-soak-authority.sh
	python3 scripts/test-go-closure-evidence-chain.py
	bash scripts/test-backup-verification-authority.sh
	bash scripts/test-governance-approval-authority.sh
	MERC_STRIPE_WEBHOOK_VERSION_SELF_TEST=1 bash scripts/stripe-webhooks.sh
	bash scripts/test-stripe-sandbox-contract.sh
	# Placement readiness is allowed to print NOT_READY (exit 1) when preconditions
	# are honestly unsatisfied. The pin test asserts the gate refuses correctly and
	# cannot be env-bypassed; it does not require the programme to be READY.
	python3 scripts/validate-placement-readiness.py; \
	  pr_status=$$?; \
	  if [ $$pr_status -ne 0 ] && [ $$pr_status -ne 1 ]; then exit $$pr_status; fi
	python3 scripts/test-placement-readiness.py
	node scripts/site-build.mjs
	node scripts/test-supplier-console.mjs
	bash -n scripts/*.sh
	bash scripts/test-backup-schedule.sh
	bash scripts/verify-python-sdk-package.sh

# Release gates that are SUPPOSED to fail right now.  They mark work that is not
# engineering -- a human has to supply a value -- so they run as their own target
# rather than reddening every pull request on unrelated changes.  `make
# release-gates` is the pre-GO check; do not silence either by editing the thing
# it inspects.
release-gates:
	python3 scripts/validate-runbook-contacts.py
	python3 scripts/validate-license-register.py

# Fails until counsel clears the register.  Both catalogue models are marked
# BLOCKED in docs/THIRD_PARTY_LICENSES.md while the binary prices and serves
# them, which is a legal question rather than an engineering one -- so this runs
# as its own target rather than blocking every build, and it must not be
# silenced by editing the register.
license-register:
	python3 scripts/generate-license-inventory.py
	python3 scripts/validate-license-register.py

audit:
	cd control && go run . audit codebase --out evidence/census

loc: audit

docker-build:
	docker build -f Dockerfile.control -t cx-control .

install:
	bash scripts/install.sh $(ARGS)

uninstall:
	bash scripts/uninstall.sh $(ARGS)

backup:
	bash scripts/backup.sh

restore-drill:
	bash scripts/restore-drill.sh

backup-envelope-test:
	bash scripts/test-backup-envelope.sh

alert-delivery-test:
	bash scripts/test-alert-delivery.sh

backup-age-metric-test:
	bash scripts/test-backup-age-metric.sh

local-independent-restore:
	bash scripts/local-independent-restore.sh

offsite-independent-restore-check:
	bash scripts/offsite-independent-restore.sh --check

offsite-independent-restore:
	bash scripts/offsite-independent-restore.sh --execute

local-production-tls:
	bash scripts/local-production-rehearsal.sh

local-rollback:
	bash scripts/local-resilience-rehearsal.sh rollback

restart-storm-local:
	bash scripts/local-resilience-rehearsal.sh restart-storm

technical-exercises:
	bash scripts/technical-exercises.sh

recovery-suite:
	bash scripts/recovery-suite.sh

alert-check:
	bash scripts/cx release alert-check

alert-page:
	bash scripts/cx release alert-page

render-staging:
	bash scripts/cx release render-staging

validate-staging:
	bash scripts/cx release validate-staging

soak-15m:
	bash scripts/local-resilience-rehearsal.sh soak --duration 900 --interval 30

soak-2h:
	bash scripts/local-resilience-rehearsal.sh soak --duration 7200 --interval 60

soak-24h:
	bash scripts/local-resilience-rehearsal.sh soak --duration 86400 --interval 60

soak-24h-persistent:
	bash scripts/start-local-soak-24h.sh start

soak-24h-status:
	bash scripts/start-local-soak-24h.sh status

release-doctor:
	bash scripts/release-doctor.sh $(if $(CHECK),--check $(CHECK),)

stripe-simulate:
	mkdir -p evidence/autonomous
	@tmp="$$(mktemp $${TMPDIR:-/tmp}/merc-payment-sim.XXXXXX.json)"; \
	  (cd control && go run . release stripe-simulate --sequences 4096) > "$$tmp"; \
	  python3 scripts/write-bound-evidence.py \
	    --out evidence/autonomous/payment-simulator.json \
	    --harness 'control/stripe_simulator.go (release stripe-simulate)' \
	    --payload-file "$$tmp" \
	    --build-binary control/stripe_simulator.go \
	    --exact-config 'deterministic stripe simulator; sequences=4096' \
	    --raw-samples 'embedded generated_sequences and scenario outcomes' \
	    --model-na 'payment simulator does not load model weights' \
	    --image-na 'no container image in this measurement' \
	    --corpus-na 'no external corpus'; \
	  rm -f "$$tmp"
	jq -e '.status == "SIMULATED PASS" and .evidence_label == "SIMULATED" and .generated_sequences.count == 4096 and .binding_status == "BOUND"' evidence/autonomous/payment-simulator.json >/dev/null

stripe-check:
	bash scripts/stripe-sandbox.sh check

stripe-matrix:
	bash scripts/stripe-sandbox.sh matrix

stripe-nonconnect:
	bash scripts/stripe-sandbox-nonconnect.sh

# Local alpha security suite. Drives Server.Routes() plus authority, containment,
# supply-chain and secret probes. Exits non-zero on any finding. Does not touch
# the staging droplet.
alpha-security:
	python3 scripts/alpha-security-suite.py

secret-audit:
	@set -e; tmp="$$(mktemp ops/stripe-secret-exposure.XXXXXX)"; \
	  python3 scripts/secret-exposure-audit.py --ci > "$$tmp"; \
	  mv "$$tmp" ops/stripe-secret-exposure.json
	jq -e '.secret_values_printed == false and .live_key_exposure == "not detected"' ops/stripe-secret-exposure.json >/dev/null

approvals-check:
	cd control && go run . release approvals-check $(if $(BUNDLE),--bundle $(BUNDLE),)

# Mutation testing: injects deliberate defects into the money and reuse paths
# and asserts the suite FAILS for each. A surviving mutation is a hole in the
# tests. Kept out of `ci` because the oracle whole-suite strategy re-runs the
# package suite once per mutation; the bare default is the adaptive contract path.
# The capability inventory exercises every lane without minting candidate
# authority. Exit 2 means exact-candidate canary proof remains incomplete.
# Blind-drop the RunPod and Stripe TEST credentials the inventory needs. Prompts
# with hidden input, verifies each key against the live API, refuses a live
# Stripe key, and writes a chmod-600 gitignored file. Never prints a secret.
credentials:
	bash scripts/merc-credentials.sh

credentials-check:
	bash scripts/merc-credentials.sh --check

# Legacy name retained for operator compatibility. This target inventories
# capability evidence; exact-candidate authority is go-closure-canary-rehearsal.
private-canary:
	python3 scripts/private-canary.py --out evidence/canary/private-canary.json

mutation-test:
	bash scripts/mutation-test.sh

# Every candidate mutation in isolated worktrees/databases. The default is an
# audited adaptive strategy: an observed source contract first, then an exact
# fresh database clone for any unit survivor. It leaves the candidate source
# tree untouched; the normal `mutation-full` gate tier (also adaptive) enforces
# the calibrated sub-five-minute candidate budget, while this standalone target
# retains its explicit environment-configurable ceiling. The `mutation-deep`
# tier is the only gate path that selects the oracle whole-suite strategy.
mutation-test-parallel:
	bash scripts/mutation-test-parallel.sh

# Tiered normal workflow. These all retain the same mutation/restoration rules;
# only the selected authority surface and hard wall-clock budget differ.
mutation-fast:
	bash scripts/mutation-gate.sh fast

mutation-authority:
	bash scripts/mutation-gate.sh authority

mutation-full:
	bash scripts/mutation-gate.sh full

# Deep redundancy is for a dedicated nightly/release validation worktree.
mutation-deep:
	bash scripts/mutation-gate.sh deep

# The local gate that makes pushing an unverified tree fail closed.
#
# Runs sequentially: worktree validation, targeted authority tests, the mutation
# suite, a CONTENT proof that the mutation runner restored the tree, full CI, the
# race suite, and a final check that HEAD did not move — then writes a receipt
# bound to that exact commit. Never runs CI while mutation tooling is modifying
# the same tree, because the steps are a program rather than a habit.
#
# Install the hook once so a push without a matching receipt is refused:
#   git config core.hooksPath .githooks
#
# Remote CI remains authoritative. This exists because a knowingly red tree was
# pushed once from a shell pipeline whose failure mode was "push anyway".
checkpoint:
	cd control && MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" go run . dev checkpoint

checkpoint-verify:
	cd control && go run . dev checkpoint-verify

# Official-SDK conformance: drives merc's realtime surface with the real
# `openai` Python and JavaScript clients, not merc's own. merc agreeing with
# merc proves self-consistency; the claim being made to buyers is that code
# already written against `openai` works by changing base_url, and only the
# official clients can show that.
#
# Kept out of `ci` because it needs both SDKs installed and pinned. It fails
# loudly on missing configuration rather than skipping: a conformance target
# that quietly passes when it ran nothing is worse than no target.
realtime-sdk-conformance:
	@test -n "$(OPENAI_PYTHON)" || { echo "set OPENAI_PYTHON=/path/to/venv/bin/python"; exit 2; }
	@test -n "$(OPENAI_NODE_MODULE)" || { echo "set OPENAI_NODE_MODULE=/path/to/node_modules/openai/index.mjs"; exit 2; }
	@test -n "$(OPENAI_NODE_VERSION)" || { echo "set OPENAI_NODE_VERSION=<installed openai version>"; exit 2; }
	cd control && MERC_TEST_OPENAI_PYTHON="$(OPENAI_PYTHON)" \
	  MERC_TEST_OPENAI_NODE="$$(command -v node)" \
	  MERC_TEST_OPENAI_NODE_MODULE="$(OPENAI_NODE_MODULE)" \
	  MERC_TEST_OPENAI_NODE_VERSION="$(OPENAI_NODE_VERSION)" \
	  go test -count=1 -v -run TestRealtimeStreamContractVerificationSettlementAndReceipt .

# Production droplet deploy. Runs ON the droplet, not from here. Preflight,
# backup, deploy, off-box verify, auto-rollback. --dry-run changes nothing.
droplet-deploy:
	@echo "Run this ON the droplet, not from a workstation:"
	@echo "  scp scripts/droplet-deploy.sh root@<droplet>:/opt/merc/"
	@echo "  ssh root@<droplet> 'cd /opt/merc && bash droplet-deploy.sh --dry-run'"
