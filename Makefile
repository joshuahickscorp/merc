ifneq (,$(wildcard .env))
include .env
export
endif

DATABASE_URL ?= postgres://cx:cx@localhost:5432/cx?sslmode=disable

.PHONY: up down dev-up dev-down migrate seed control agent-run agent-bench prove-local metrics build fmt test ci audit loc docker-build install uninstall backup restore-drill backup-envelope-test release-doctor

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

release-doctor:
	bash scripts/release-doctor.sh $(if $(CHECK),--check $(CHECK),)
