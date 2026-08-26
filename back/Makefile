# upcontrol backend Makefile. The single gate is `make check`.
#
# GOEXPERIMENT=jsonv2 is baked in everywhere (§1): it is the runtime's JSON v2
# implementation, default from Go 1.27 onward. Until then we set it explicitly
# so the encoding/json/v2 import resolves the same way in every environment.
GOEXPERIMENT ?= jsonv2
export GOEXPERIMENT

GO ?= go
PKG := ./...

# Pinned codegen/migration tools live in go.mod's `tool` block and run via
# `go tool X` — no global installs, no "works on my machine".
SQLC       := $(GO) tool sqlc
OAPI       := $(GO) tool oapi-codegen
BUF        := $(GO) tool buf
GOOSE      := $(GO) tool goose
LINT       := $(GO) tool golangci-lint

# Packages in the "Logic" zone (§0.1): every one of these MUST ship a _test.go.
LOGIC_PKGS := \
	./internal/ingest/... \
	./internal/ring/... \
	./internal/detect/... \
	./internal/incident/... \
	./internal/deliver/... \
	./internal/probe/... \
	./internal/account/... \
	./internal/analytics/...

.PHONY: all generate build vet lint test cover test-integration test-contract test-ha check tidy fmt

all: build

## generate: regenerate every contract artifact. `git diff --exit-code` after
## this must be empty (gate, §6). Order matters: schema.sql is derived from the
## migration before sqlc reads it.
generate:
	@sh scripts/extract-schema.sh
	$(SQLC) generate
	$(OAPI) -package api -generate types,strict-server,spec -o gen/api/api.gen.go api/openapi.yaml
	$(BUF) generate

## build: compile the three binaries.
build:
	$(GO) build ./cmd/...

## vet: go vet across the module.
vet:
	$(GO) vet $(PKG)

## lint: golangci-lint (invariants 1,2,4 via depguard; invariant 8 via slog-any
## ban) plus a check that every Logic-zone package has a _test.go (§1.1a).
lint: .golangci.yml
	$(LINT) run
	@sh scripts/check-logic-tests.sh "$(LOGIC_PKGS)"

## test: short unit tests, the per-commit gate.
test:
	$(GO) test -short $(PKG)

# testcontainers talks to the Docker daemon directly and does NOT read the
# docker CLI's active context, so on a machine whose daemon is not at
# /var/run/docker.sock (OrbStack, Colima, rootless Docker) every container test
# fails with "rootless Docker not found, failed to create Docker provider".
# Resolving the context here makes `make test-integration` work out of the box;
# an explicitly-set DOCKER_HOST still wins.
DOCKER_HOST ?= $(shell docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null)
export DOCKER_HOST

## test-integration: testcontainers-backed storage tests, run per-PR.
test-integration:
	$(GO) test -tags=integration ./internal/storage/...

## test-contract: golden-JSON contract test against front's mockData shapes.
test-contract:
	$(GO) test -tags=contract ./internal/api/...

## test-ha: the two-instance horizontal test (invariants 5, 6).
test-ha:
	$(GO) test -tags=ha ./test/ha/...

## cover: report coverage over the Logic zone (§0.1) and fail if it dropped
## below the saved baseline. gen/** and cmd/** are excluded by design
## (invariant 7: generated code is not unit-tested), so only the hand-written
## Logic zone is gated.
COVER_OUT := coverage.out
COVER_BASELINE := .coverage-baseline
cover:
	@sh scripts/cover.sh "$(LOGIC_PKGS)" "$(COVER_OUT)" "$(COVER_BASELINE)"

## check: the single gate (§6). vet + lint + test.
check: vet lint test
	@echo "make check: OK"

## tidy: go mod tidy, used by the generate gate to keep go.sum honest.
tidy:
	$(GO) mod tidy

## fmt: format all Go (formatters are part of the lint pass too).
fmt:
	@gofmt -w .
	@goimports -w -local go.upcontrol.io/back .
