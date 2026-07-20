DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

.PHONY: up down dev-up dev-down migrate seed control agent-run agent-bench agent-characterize prove-local metrics build fmt test ci audit loc docker-build install uninstall backup restore-drill backup-envelope-test local-independent-restore local-production-tls local-rollback restart-storm-local technical-exercises alert-check alert-page render-staging validate-staging soak-15m soak-2h soak-24h soak-24h-persistent soak-24h-status release-doctor stripe-simulate stripe-check stripe-matrix secret-audit approvals-check

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

test:
	cd control && go test ./...
	cd agent && cargo test
	bash scripts/verify-python-sdk-package.sh

ci:
	cd control && test -z "$$(gofmt -l .)" && go vet ./... && go test ./...
	cd agent && cargo fmt --all -- --check && cargo clippy --all-targets -- -D warnings && cargo test
	python3 -m json.tool proto/manifest.schema.json >/dev/null
	python3 -m json.tool ops/governance-approval-bundle.schema.json >/dev/null
	python3 scripts/validate-authorization-matrix.py
	python3 scripts/validate-independent-reviews.py
	python3 scripts/validate-governance.py
	python3 scripts/validate-readiness.py
	node scripts/site-build.mjs
	bash scripts/verify-python-sdk-package.sh

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
