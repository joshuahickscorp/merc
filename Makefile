DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

.PHONY: realtime-sdk-conformance up down dev-up dev-down test-unit license-register release-gates alert-delivery-test backup-age-metric-test migrate seed control agent-run agent-bench agent-characterize prove-local metrics build fmt test ci audit loc docker-build install uninstall backup restore-drill backup-envelope-test local-independent-restore local-production-tls local-rollback restart-storm-local technical-exercises alert-check alert-page render-staging validate-staging soak-15m soak-2h soak-24h soak-24h-persistent soak-24h-status release-doctor stripe-simulate stripe-check stripe-matrix secret-audit approvals-check mutation-test

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

test:
	cd control && MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" go test ./...
	cd agent && cargo test
	bash scripts/verify-python-sdk-package.sh

# The fast loop: unit tests only, database suite explicitly opted out.  CI never
# sets MERC_ALLOW_SKIPPING_DB_TESTS, so the money and scheduling tests cannot stop
# running there without someone noticing.
test-unit:
	cd control && MERC_ALLOW_SKIPPING_DB_TESTS=1 go test ./...

ci:
	cd control && test -z "$$(gofmt -l .)" && go vet ./... && \
	  MERC_TEST_DATABASE_URL="$(MERC_TEST_DATABASE_URL)" go test ./...
	@bash scripts/assert-no-test-skips.sh
	cd agent && cargo fmt --all -- --check && cargo clippy --all-targets -- -D warnings && cargo test
	python3 -m json.tool proto/manifest.schema.json >/dev/null
	python3 -m json.tool ops/governance-approval-bundle.schema.json >/dev/null
	python3 scripts/validate-authorization-matrix.py
	python3 scripts/validate-independent-reviews.py
	python3 scripts/validate-governance.py
	python3 scripts/validate-readiness.py
	python3 scripts/assert-soak-claims.py
	python3 scripts/validate-claim-surfaces.py
	python3 scripts/rename-residue-audit.py
	python3 scripts/test-bench-accounting.py
	bash scripts/test-readiness-gaming.sh
	bash scripts/test-agent-review-gaming.sh
	bash scripts/test-technical-exercises-fail-closed.sh
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
	python3 scripts/validate-license-register.py

audit:
	cd control && go run . audit codebase --out census

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

local-production-tls:
	bash scripts/local-production-rehearsal.sh

local-rollback:
	bash scripts/local-resilience-rehearsal.sh rollback

restart-storm-local:
	bash scripts/local-resilience-rehearsal.sh restart-storm

technical-exercises:
	bash scripts/technical-exercises.sh

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
	cd control && go run . release stripe-simulate --sequences 4096 > ../evidence/autonomous/payment-simulator.json.tmp
	mv evidence/autonomous/payment-simulator.json.tmp evidence/autonomous/payment-simulator.json
	jq -e '.status == "SIMULATED PASS" and .evidence_label == "SIMULATED" and .generated_sequences.count == 4096' evidence/autonomous/payment-simulator.json >/dev/null

stripe-check:
	bash scripts/stripe-sandbox.sh check

stripe-matrix:
	bash scripts/stripe-sandbox.sh matrix

secret-audit:
	@set -e; tmp="$$(mktemp ops/stripe-secret-exposure.XXXXXX)"; \
	  python3 scripts/secret-exposure-audit.py --ci > "$$tmp"; \
	  mv "$$tmp" ops/stripe-secret-exposure.json
	jq -e '.secret_values_printed == false and .live_key_exposure == "not detected"' ops/stripe-secret-exposure.json >/dev/null

approvals-check:
	cd control && go run . release approvals-check $(if $(BUNDLE),--bundle $(BUNDLE),)

# Mutation testing: injects deliberate defects into the money and reuse paths
# and asserts the suite FAILS for each. A surviving mutation is a hole in the
# tests. Kept out of `ci` because it runs the full suite once per mutation.
mutation-test:
	bash scripts/mutation-test.sh

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
