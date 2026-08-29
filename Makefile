DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable
COMPOSE_FILE ?= ops/deploy/docker-compose.yml

.PHONY: credentials credentials-check droplet-deploy private-canary realtime-sdk-conformance up down dev-up dev-down test-qualification test-unit test-normal test-integration test-expensive test-full test-certification certify license-register release-gates alert-delivery-test backup-age-metric-test migrate seed control agent-run agent-bench agent-characterize prove-local metrics build fmt test ci audit loc docker-build install uninstall backup restore-drill backup-envelope-test local-independent-restore offsite-independent-restore offsite-independent-restore-check offsite-droplet-restore offsite-droplet-restore-check local-production-tls local-rollback restart-storm-local technical-exercises recovery-suite alert-check alert-page render-staging validate-staging soak-15m soak-2h soak-24h soak-24h-persistent soak-24h-status release-doctor stripe-simulate stripe-check stripe-matrix secret-audit approvals-check mutation-test mutation-test-parallel mutation-fast mutation-authority mutation-full mutation-deep alpha-security stripe-nonconnect stripe-endpoint-subscriptions alpha-e2e-rehearsal test-money-contract

up:
	docker compose -f "$(COMPOSE_FILE)" up -d --build

down:
	docker compose -f "$(COMPOSE_FILE)" down

dev-up:
	docker compose -f "$(COMPOSE_FILE)" up -d postgres minio createbuckets

dev-down:
	docker compose -f "$(COMPOSE_FILE)" down

migrate:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 --single-transaction -f src/control/schema.sql

seed:
	cd src/control && go run . seed

control:
	cd src/control && go run .

agent-run:
	cd src/agent && cargo run --release -- run

agent-bench:
	cd src/agent && cargo run --release -- bench

agent-characterize:
	cd src/agent && cargo run --release -- characterize

prove-local:
	cd src/control && go run . prove --full

metrics:
	curl -fsS localhost:8080/metrics

build:
	cd src/control && go build ./...
	cd src/agent && cargo build

fmt:
	cd src/control && gofmt -w .
	cd src/agent && cargo fmt --all

# MERC_TEST_DATABASE_URL defaults to the `make dev-up` Compose database so that
# `make ci` runs the integration suite instead of skipping it.  Override it to
# point at another instance; the tests fail closed when it is unreachable.
MERC_TEST_DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

# Object storage and a local inference engine are passed THROUGH when the
# environment provides them, so a developer machine that has `make dev-up` MinIO
# and a llama-server actually runs the artifact, chain and failure-matrix tests
# instead of skipping them. The CI lane has neither and skips them; every one of
# those skips is named in ops/scripts/allowed-test-skips.txt, which is what stops the
# skipping from being invisible.
CI_TEST_ENV = MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" \
  MERC_TEST_S3_ENDPOINT="$(MERC_TEST_S3_ENDPOINT)" \
  MERC_TEST_S3_BUCKET="$(MERC_TEST_S3_BUCKET)" \
  MERC_TEST_S3_ACCESS_KEY="$(MERC_TEST_S3_ACCESS_KEY)" \
  MERC_TEST_S3_SECRET_KEY="$(MERC_TEST_S3_SECRET_KEY)" \
  MERC_LLAMA_EMBED_URL="$(MERC_LLAMA_EMBED_URL)"

# Isolated databases clone a schema-stamped template (CREATE DATABASE TEMPLATE)
# instead of applying src/control/schema.sql per test. ensure-schema-template.sh
# rebuilds merc_schema_ddl_<sha16> when schema.sql changes; a stale name is refused.
# -parallel 16 matches the mutation-gate worker default.
#
# -timeout 45m, not the 10m default. The suite grew agent-PROCESS tests: two
# merc-agent binaries cold-load and benchmark both retained models before they
# register, so it runs about fourteen minutes on a host that has object storage
# and a local engine. The default killed `make ci` mid-run with a ten-minute
# panic, which read as a hung test rather than as a budget.
#
# Test tiers are explicit so a green local loop cannot be mistaken for release
# certification.  The qualification tier is deliberately small and deterministic;
# normal adds the package suites; integration requires the real DB/object-store
# and agent loop; certification runs every release gate below.
test-qualification:
	cd src/control && MERC_ALLOW_SKIPPING_DB_TESTS=1 go test -short -count=1 -run '^(TestMoneyUSDDomainBounds|TestMoneyDomainRejectsOversizeValues|TestExactMoneyRefusesToMixCurrencies|TestExactMoneyRefusesToOverflowInsteadOfWrapping|TestCurrencyExponentRecordedNotAssumedTwoDecimals|TestActivationPolicySeedsFromTheDocumentAndProjectsBack|TestAuthorizationMatrixProtectedRoutesRejectAnonymousAndWrongCredentialNamespace|TestEvidenceEnvelopeSealsAndValidates|TestEvidenceEnvelopeTamperRejection|TestLiabilityAuthorityValidateRequiresDigest|TestNoRawLedgerInsertsOutsideWriter|TestQualityContractsEmbedAndGenerationAreHonest|TestSubmissionIdempotencyKeyContract|TestQuoteAndSubmitRejectUnknownObjective|TestIdentityFieldsCannotBeConfused|TestReceiptBindingDigestTracksTheRequest|TestWorkloadDecisionIsSingleJobFreezeNotGraphProjection|TestWorkloadDecisionRejectsTampering)$$' .

# Compatibility name retained for the historical broad local-stack loop.
test:
	@template="$$(MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash ops/scripts/ensure-schema-template.sh)"; \
	cd src/control && $(CI_TEST_ENV) MERC_ISOLATED_TEST_DB_TEMPLATE="$$template" bash ../../ops/scripts/with-isolated-test-storage.sh bash ../../ops/scripts/with-isolated-test-db.sh go test -timeout 45m -parallel 16 ./...
	cd src/agent && cargo test
	bash ops/scripts/verify-python-sdk-package.sh

# Agent-subprocess public-API loop. Requires `cargo build --release` in src/agent/
# so merc-agent exists; object storage comes from with-isolated-test-storage.sh.
test-integration:
	@template="$$(MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash ops/scripts/ensure-schema-template.sh)"; \
	cd src/control && $(CI_TEST_ENV) MERC_ISOLATED_TEST_DB_TEMPLATE="$$template" bash ../../ops/scripts/with-isolated-test-storage.sh bash ../../ops/scripts/with-isolated-test-db.sh go test -timeout 45m -tags=integration -parallel 16 -run '^TestFirstCompleteLoopThroughThePublicAPI$$' ./...

# Tier 1: deterministic normal suite. Database-dependent cases are explicitly
# skipped when no isolated database is supplied; the release/CI tier does not
# use this opt-out. No integration result is synthesized by this target.
test-normal:
	cd src/control && MERC_ALLOW_SKIPPING_DB_TESTS=1 go test -short -count=1 ./...
	cd src/agent && cargo test --quiet
	bash ops/scripts/verify-python-sdk-package.sh

# Historical name for the Go portion of Tier 1.
test-unit:
	cd src/control && MERC_ALLOW_SKIPPING_DB_TESTS=1 go test -short -count=1 ./...

# Tier 2: external-service and cross-process checks. These fail or report an
# explicit unavailable dependency; they must not be replaced with stand-ins.
test-expensive: test test-integration

# Tier 3: full certification/release gate. This is intentionally the same
# command used by CI so the local certification surface cannot drift.
test-full: ci
test-certification: ci
certify: ci

# The suite is recorded ONCE, as JSON, and both the human summary and the
# skip gate read that record.
#
# assert-no-test-skips.sh used to run the whole suite again: fourteen extra
# minutes on a host with object storage and a local engine, and the second run
# inherited the first one's rows in the shared database, so it failed on tests
# that had just passed. Same 45m budget as `test` above, same reason.
CI_TEST_JSON = $(CURDIR)/.ci-test.json

# Every step still fails the target when it is red. Failures are collected so
# `make ci` reaches the last gate instead of stopping at the first one. The
# fail list is kept in the shell (not a worktree file) so an untracked receipt
# cannot trip test-release-image-boots.sh.
ci:
	@fails=""; \
	run() { \
	  name="$$1"; shift; \
	  echo "==== CI $$name ===="; \
	  if sh -c "$$1"; then echo "==== CI PASS $$name ===="; \
	  else echo "==== CI FAIL $$name ===="; fails="$$fails $$name"; fi; \
	}; \
	run cargo-build 'cd src/agent && cargo build --release'; \
	run gofmt-vet 'cd src/control && test -z "$$(gofmt -l .)" && go vet ./...'; \
	run hydrate-lfs 'bash ops/scripts/hydrate-release-lfs.sh'; \
	run go-tests '$(CI_TEST_ENV) bash "$(CURDIR)/ops/scripts/run-ci-control-tests.sh" "$(CI_TEST_JSON)"; go_rc=$$?; python3 "$(CURDIR)/ops/scripts/summarize-go-test-json.py" "$(CI_TEST_JSON)"; exit $$go_rc'; \
	run assert-no-test-skips 'bash ops/scripts/assert-no-test-skips.sh "$(CI_TEST_JSON)"'; \
	run image-boots 'rm -f "$(CI_TEST_JSON).fast" "$(CI_TEST_JSON).fast.err" "$(CI_TEST_JSON).integration" "$(CI_TEST_JSON).integration.err"; bash ops/scripts/test-release-image-boots.sh'; \
	run image-contents 'bash ops/scripts/test-release-image-contents.sh'; \
	run cargo-fmt 'cd src/agent && cargo fmt --all -- --check'; \
	run cargo-clippy 'cd src/agent && cargo clippy --all-targets -- -D warnings'; \
	run cargo-test 'cd src/agent && cargo test'; \
	run json-manifest 'python3 -m json.tool clients/proto/manifest.schema.json >/dev/null'; \
	run json-governance-schema 'python3 -m json.tool ops/governance-approval-bundle.schema.json >/dev/null'; \
	run json-payment-schema 'python3 -m json.tool ops/live-payment-activation.schema.json >/dev/null'; \
	run runpod-spend-self 'python3 ops/scripts/runpod-spend-guard.py --self-test'; \
	run runpod-orphan 'python3 ops/scripts/test-runpod-orphan-reconcile.py'; \
	run runpod-revalidate 'python3 ops/scripts/runpod-spend-guard.py revalidate'; \
	run authorization-matrix 'python3 ops/scripts/validate-authorization-matrix.py'; \
	run sdk-routes 'python3 ops/scripts/validate-sdk-routes.py'; \
	run independent-reviews 'python3 ops/scripts/validate-independent-reviews.py'; \
	run governance 'python3 ops/scripts/validate-governance.py'; \
	run readiness 'python3 ops/scripts/validate-readiness.py'; \
	run soak-claims 'python3 ops/scripts/assert-soak-claims.py'; \
	run claim-surfaces 'python3 ops/scripts/validate-claim-surfaces.py'; \
	run rename-residue 'python3 ops/scripts/rename-residue-audit.py'; \
	run agent-install-sig 'bash ops/scripts/test-agent-install-signature.sh'; \
	run agent-uninstall 'bash ops/scripts/test-agent-uninstall-legacy.sh'; \
	run agent-cross-platform 'bash ops/scripts/test-agent-cross-platform-installers.sh'; \
	run repo-boundary 'python3 ops/scripts/validate-repo-boundary.py'; \
	run bench-accounting 'python3 ops/scripts/test-bench-accounting.py'; \
	run gateway-parity 'python3 ops/scripts/test-gateway-parity-receipt.py'; \
	run evidence-binding 'python3 ops/scripts/validate-evidence-binding.py'; \
	run evidence-deps 'python3 ops/scripts/test-evidence-binding-dependencies.py'; \
	run evidence-withdrawn 'python3 ops/scripts/test-evidence-binding-withdrawn.py'; \
	run lfs-corpus 'python3 ops/scripts/verify-lfs-corpus.py'; \
	run evidence-writer 'python3 ops/scripts/test-evidence-writer-bypass.py'; \
	run mutation-parallel 'bash ops/scripts/test-mutation-test-parallel.sh'; \
	run mutation-preflight 'bash ops/scripts/test-mutation-load-preflight.sh'; \
	run mutation-contracts 'python3 ops/scripts/test-mutation-test-contracts.py'; \
	run mutation-observer 'python3 ops/scripts/test-mutation-contract-observer.py'; \
	run mutation-suite-obs 'python3 ops/scripts/test-mutation-suite-observer.py'; \
	run mutation-preflight-cache 'python3 ops/scripts/test-mutation-preflight-cache.py'; \
	run mutation-manifest 'python3 ops/scripts/test-mutation-manifest.py'; \
	run isolated-db 'bash ops/scripts/test-with-isolated-test-db.sh'; \
	run schema-template 'MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash ops/scripts/test-schema-template.sh'; \
	run mutation-gate 'bash ops/scripts/test-mutation-gate.sh'; \
	run mutation-oracle 'bash ops/scripts/test-mutation-oracle-strategy.sh'; \
	run readiness-gaming 'bash ops/scripts/test-readiness-gaming.sh'; \
	run agent-review-gaming 'bash ops/scripts/test-agent-review-gaming.sh'; \
	run technical-exercises-fc 'bash ops/scripts/test-technical-exercises-fail-closed.sh'; \
	run canary-gaming 'bash ops/scripts/test-canary-gaming.sh'; \
	run canary-receipt 'bash ops/scripts/test-canary-scenario-receipt.sh'; \
	run canary-db 'MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash ops/scripts/test-canary-database-corroboration.sh'; \
	run agent-restart 'MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" bash ops/scripts/test-agent-restart-authority.sh'; \
	run go-closure-soak 'bash ops/scripts/test-go-closure-soak-authority.sh'; \
	run go-closure-chain 'python3 ops/scripts/test-go-closure-evidence-chain.py'; \
	run backup-verify-auth 'bash ops/scripts/test-backup-verification-authority.sh'; \
	run gov-approval-auth 'bash ops/scripts/test-governance-approval-authority.sh'; \
	run stripe-webhooks 'MERC_STRIPE_WEBHOOK_VERSION_SELF_TEST=1 bash ops/scripts/stripe-webhooks.sh'; \
	run stripe-sandbox-contract 'bash ops/scripts/test-stripe-sandbox-contract.sh'; \
	run placement-readiness 'python3 ops/scripts/validate-placement-readiness.py; pr=$$?; if [ $$pr -ne 0 ] && [ $$pr -ne 1 ]; then exit $$pr; fi'; \
	run placement-test 'python3 ops/scripts/test-placement-readiness.py'; \
	run site-build 'node ops/scripts/site-build.mjs'; \
	run supplier-console 'node ops/scripts/test-supplier-console.mjs'; \
	run agent-seatbelt 'bash ops/scripts/test-agent-package-contains-seatbelt.sh'; \
	run recovery-receipts-fc 'bash ops/scripts/test-recovery-receipts-fail-closed.sh'; \
	run private-canary-integrity 'bash ops/scripts/test-private-canary-integrity.sh'; \
	run backup-envelope 'bash ops/scripts/test-backup-envelope.sh'; \
	run bash-n 'bash -n ops/scripts/*.sh'; \
	run backup-schedule 'bash ops/scripts/test-backup-schedule.sh'; \
	run python-sdk 'bash ops/scripts/verify-python-sdk-package.sh'; \
	if [ -n "$$fails" ]; then echo "CI remaining failures:$$fails"; exit 1; fi; \
	echo "CI PASS"

# Release gates that are SUPPOSED to fail right now.  They mark work that is not
# engineering -- a human has to supply a value -- so they run as their own target
# rather than reddening every pull request on unrelated changes.  `make
# release-gates` is the pre-GO check; do not silence either by editing the thing
# it inspects.
release-gates:
	python3 ops/scripts/validate-runbook-contacts.py
	python3 ops/scripts/validate-license-register.py

# Fails until counsel clears the register.  Both catalogue models are marked
# BLOCKED in docs/THIRD_PARTY_LICENSES.md while the binary prices and serves
# them, which is a legal question rather than an engineering one -- so this runs
# as its own target rather than blocking every build, and it must not be
# silenced by editing the register.
license-register:
	python3 ops/scripts/generate-license-inventory.py
	python3 ops/scripts/validate-license-register.py

audit:
	cd src/control && go run . audit codebase --out ../../evidence/census

loc: audit

docker-build:
	docker build -f ops/deploy/Dockerfile.control -t cx-control .

install:
	bash ops/scripts/install.sh $(ARGS)

uninstall:
	bash ops/scripts/uninstall.sh $(ARGS)

backup:
	bash ops/scripts/backup.sh

restore-drill:
	bash ops/scripts/restore-drill.sh

backup-envelope-test:
	bash ops/scripts/test-backup-envelope.sh

alert-delivery-test:
	bash ops/scripts/test-alert-delivery.sh

backup-age-metric-test:
	bash ops/scripts/test-backup-age-metric.sh

local-independent-restore:
	bash ops/scripts/local-independent-restore.sh

offsite-independent-restore-check:
	bash ops/scripts/offsite-independent-restore.sh --check

offsite-independent-restore:
	bash ops/scripts/offsite-independent-restore.sh --execute

offsite-droplet-restore-check:
	bash ops/scripts/offsite-independent-restore.sh --check --source droplet

offsite-droplet-restore:
	bash ops/scripts/offsite-independent-restore.sh --execute --source droplet

local-production-tls:
	bash ops/scripts/local-production-rehearsal.sh

local-rollback:
	bash ops/scripts/local-resilience-rehearsal.sh rollback

restart-storm-local:
	bash ops/scripts/local-resilience-rehearsal.sh restart-storm

technical-exercises:
	bash ops/scripts/technical-exercises.sh

recovery-suite:
	bash ops/scripts/recovery-suite.sh

alert-check:
	bash ops/scripts/cx release alert-check

alert-page:
	bash ops/scripts/cx release alert-page

render-staging:
	bash ops/scripts/cx release render-staging

validate-staging:
	bash ops/scripts/cx release validate-staging

soak-15m:
	bash ops/scripts/local-resilience-rehearsal.sh soak --duration 900 --interval 30

soak-2h:
	bash ops/scripts/local-resilience-rehearsal.sh soak --duration 7200 --interval 60

soak-24h:
	bash ops/scripts/local-resilience-rehearsal.sh soak --duration 86400 --interval 60

soak-24h-persistent:
	bash ops/scripts/start-local-soak-24h.sh start

soak-24h-status:
	bash ops/scripts/start-local-soak-24h.sh status

release-doctor:
	bash ops/scripts/release-doctor.sh $(if $(CHECK),--check $(CHECK),)

stripe-simulate:
	python3 ops/scripts/produce-payment-simulator.py
	jq -e '.status == "SIMULATED PASS" and .evidence_label == "SIMULATED" and .generated_sequences.count == 4096 and .binding_status == "BOUND"' evidence/autonomous/payment-simulator.json >/dev/null

test-money-contract:
	@# The L2 Stripe matrix and the Connect handler need the REAL dashboard pair
	@# plus a real sk_test_ key: the Connect route answers 503 without payment
	@# authority, and distinct whsec_ secrets are the authority boundary itself.
	@# An earlier pass made these tests pass by substituting synthetic secrets,
	@# which is why the requirement is spelled out here instead of assumed.
	@set -e; \
	  K="$$(awk -F= '/^export STRIPE_SECRET_KEY=/{print $$2}' .merc-credentials.env)"; \
	  B="$$(awk -F= '/^STRIPE_WEBHOOK_SECRET=/{print $$2}' .env)"; \
	  C="$$(awk -F= '/^MERC_CONNECT_WEBHOOK_SECRET=/{print $$2}' .env)"; \
	  case "$$K" in sk_test_*|rk_test_*) ;; *) echo "refusing: STRIPE_SECRET_KEY is not a test key" >&2; exit 1;; esac; \
	  [ -n "$$B" ] && [ -n "$$C" ] && [ "$$B" != "$$C" ] || { echo "refusing: need distinct billing and Connect whsec_ secrets" >&2; exit 1; }; \
	  cd src/control && STRIPE_SECRET_KEY="$$K" STRIPE_WEBHOOK_SECRET="$$B" MERC_CONNECT_WEBHOOK_SECRET="$$C" \
	    MERC_TEST_DATABASE_URL="$${MERC_TEST_DATABASE_URL:-postgres://cx:cx@192.168.148.2:5432/cx?sslmode=disable}" \
	    go test . -count=1 -run 'TestL2Stripe|TestLiveActivation|TestAdvertisedSurface|TestDocumentAdvertised'

stripe-endpoint-subscriptions:
	python3 ops/scripts/validate-stripe-endpoint-subscriptions.py

stripe-check:
	bash ops/scripts/stripe-sandbox.sh check

stripe-matrix:
	bash ops/scripts/stripe-sandbox.sh matrix

stripe-nonconnect:
	bash ops/scripts/stripe-sandbox-nonconnect.sh

# Local alpha security suite. Drives Server.Routes() plus authority, containment,
# supply-chain and secret probes. Exits non-zero on any finding. Does not touch
# the staging droplet.
alpha-security:
	python3 ops/scripts/alpha-security-suite.py

# Operator-controlled live rehearsal against https://mercmerc.net.
# Writes evidence/canary/l11-p1-canary-rehearsal-*.json. Does not,
# and cannot, flip EXTERNAL_ALPHA_PROVEN. Needs a session in
# .artifacts/alpha-e2e/session.json or MERC_CANARY_BUYER_API_KEY.
alpha-e2e-rehearsal:
	python3 ops/scripts/alpha-e2e-rehearsal.py run

secret-audit:
	@set -e; tmp="$$(mktemp ops/stripe-secret-exposure.XXXXXX)"; \
	  python3 ops/scripts/secret-exposure-audit.py --ci > "$$tmp"; \
	  mv "$$tmp" ops/stripe-secret-exposure.json
	jq -e '.secret_values_printed == false and .live_key_exposure == "not detected"' ops/stripe-secret-exposure.json >/dev/null

approvals-check:
	cd src/control && go run . release approvals-check $(if $(BUNDLE),--bundle $(BUNDLE),)

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
	bash ops/scripts/merc-credentials.sh

credentials-check:
	bash ops/scripts/merc-credentials.sh --check

# Legacy name retained for operator compatibility. This target inventories
# capability evidence; exact-candidate authority is go-closure-canary-rehearsal.
private-canary:
	python3 ops/scripts/private-canary.py --out evidence/canary/private-canary.json

mutation-test:
	bash ops/scripts/mutation-test.sh

# Every candidate mutation in isolated worktrees/databases. The default is an
# audited adaptive strategy: an observed source contract first, then an exact
# fresh database clone for any unit survivor. It leaves the candidate source
# tree untouched; the normal `mutation-full` gate tier (also adaptive) enforces
# the calibrated sub-five-minute candidate budget, while this standalone target
# retains its explicit environment-configurable ceiling. The `mutation-deep`
# tier is the only gate path that selects the oracle whole-suite strategy.
mutation-test-parallel:
	bash ops/scripts/mutation-test-parallel.sh

# Tiered normal workflow. These all retain the same mutation/restoration rules;
# only the selected authority surface and hard wall-clock budget differ.
mutation-fast:
	bash ops/scripts/mutation-gate.sh fast

mutation-authority:
	bash ops/scripts/mutation-gate.sh authority

mutation-full:
	bash ops/scripts/mutation-gate.sh full

# Deep redundancy is for a dedicated nightly/release validation worktree.
mutation-deep:
	bash ops/scripts/mutation-gate.sh deep

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
	cd src/control && MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" go run . dev checkpoint

checkpoint-verify:
	cd src/control && go run . dev checkpoint-verify

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
	cd src/control && MERC_TEST_OPENAI_PYTHON="$(OPENAI_PYTHON)" \
	  MERC_TEST_OPENAI_NODE="$$(command -v node)" \
	  MERC_TEST_OPENAI_NODE_MODULE="$(OPENAI_NODE_MODULE)" \
	  MERC_TEST_OPENAI_NODE_VERSION="$(OPENAI_NODE_VERSION)" \
	  go test -count=1 -v -run TestRealtimeStreamContractVerificationSettlementAndReceipt .

# Production droplet deploy. Runs ON the droplet, not from here. Preflight,
# backup, deploy, off-box verify, auto-rollback. --dry-run changes nothing.
droplet-deploy:
	@echo "Run this ON the droplet, not from a workstation:"
	@echo "  scp ops/scripts/droplet-deploy.sh root@<droplet>:/opt/merc/"
	@echo "  ssh root@<droplet> 'cd /opt/merc && bash droplet-deploy.sh --dry-run'"
