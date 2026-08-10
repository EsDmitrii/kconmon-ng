PROJECT_NAME := kconmon-ng
MODULE := github.com/EsDmitrii/kconmon-ng
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w \
	-X $(MODULE)/internal/config.Version=$(VERSION) \
	-X $(MODULE)/internal/config.Commit=$(COMMIT) \
	-X $(MODULE)/internal/config.BuildDate=$(BUILD_DATE)

BIN_DIR := bin

.PHONY: all build build-agent build-controller build-console test test-race test-cover lint fmt proto sqlc openapi clean help \
	local-up local-down local-status local-smoke local-urls

all: lint test build

## Build

build: build-agent build-controller build-console

build-agent:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/kconmon-ng-agent ./cmd/agent

build-controller:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/kconmon-ng-controller ./cmd/controller

build-console:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/kconmon-ng-console ./cmd/console

## Test

test:
	go test ./... -v -count=1

test-race:
	go test ./... -v -race -count=1

test-cover:
	go test ./... -v -race -coverprofile=coverage.txt -covermode=atomic
	go tool cover -html=coverage.txt -o coverage.html

test-fuzz:
	go test ./internal/checker/ -fuzz=. -fuzztime=30s

## Lint

# Keep in sync with .github/workflows/ci.yaml (golangci-lint-action `version:`).
# `go run` pins the exact CI version so local lint == CI lint; a system-wide
# golangci-lint of a different version has already hidden CI-only findings once.
GOLANGCI_LINT_VERSION ?= v2.10.1

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt:
	gofmt -s -w .
	goimports -w .

## Proto

proto:
	buf generate api/proto

## sqlc

# Pinned so a locally-installed sqlc of a different version cannot silently
# regenerate different code, exactly as GOLANGCI_LINT_VERSION is pinned above.
SQLC_VERSION ?= v1.31.1

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

## OpenAPI

# CI runs this and fails on a diff, so a spec edit without a regenerate cannot land; api-types.ts is committed.

openapi:
	cd web && npm run gen:api

## Helm

# Optional subcharts are pinned in Chart.lock and gitignored, so a fresh clone must fetch them first.
# The chart needs its own copy of dashboards/ to package them; this keeps the two honest.
dashboards-check:
	@diff -r dashboards charts/kconmon-ng/dashboards && echo "dashboards in sync"

helm-deps:
	helm dependency build charts/kconmon-ng

# JSON keeps the LAST of a repeated key, so a duplicate silently kills the earlier definition.
schema-lint:
	@python3 hack/schema-lint.py charts/kconmon-ng/values.schema.json

helm-lint: helm-deps dashboards-check schema-lint
	helm lint charts/kconmon-ng
	@for f in charts/kconmon-ng/ci/*.yaml; do \
		echo "--- lint $$f"; \
		helm lint charts/kconmon-ng -f "$$f" || exit 1; \
	done

# EVERY ci profile is templated as well as linted; the two are not interchangeable.
helm-template: helm-deps
	helm template kconmon-ng charts/kconmon-ng
	@for f in charts/kconmon-ng/ci/*.yaml; do \
		echo "--- template $$f"; \
		helm template kconmon-ng charts/kconmon-ng -f "$$f" >/dev/null || exit 1; \
	done

helm-package: helm-deps
	helm package charts/kconmon-ng -d dist/

## Docker

docker-build:
	docker build --target agent -t $(PROJECT_NAME)-agent:$(VERSION) .
	docker build --target controller -t $(PROJECT_NAME)-controller:$(VERSION) .
	docker build -f Dockerfile.console -t $(PROJECT_NAME)-console:$(VERSION) .

## Clean

clean:
	rm -rf $(BIN_DIR) dist/ coverage.txt coverage.html

## Local testing (minikube + Prometheus + Grafana)

local-up:
	hack/local-test.sh up

local-down:
	hack/local-test.sh down

local-status:
	hack/local-test.sh status

local-smoke:
	hack/local-test.sh smoke

local-urls:
	hack/local-test.sh urls

## Help

help:
	@echo "Available targets:"
	@echo "  build            - Build agent, controller and console binaries"
	@echo "  test             - Run unit tests"
	@echo "  test-race        - Run tests with race detector"
	@echo "  test-cover       - Run tests with coverage"
	@echo "  test-fuzz        - Run fuzz tests"
	@echo "  lint             - Run golangci-lint"
	@echo "  fmt              - Format code"
	@echo "  proto            - Generate protobuf code"
	@echo "  sqlc             - Generate database query code"
	@echo "  openapi          - Generate TS API types from docs/console-api.yaml (needs Node + web deps)"
	@echo "  helm-lint        - Lint Helm chart"
	@echo "  helm-template    - Render Helm templates"
	@echo "  helm-package     - Package Helm chart"
	@echo "  docker-build     - Build Docker images"
	@echo "  clean            - Remove build artifacts"
	@echo "  local-up         - Start minikube + Prometheus + Grafana + kconmon-ng"
	@echo "  local-down       - Delete minikube cluster"
	@echo "  local-status     - Show cluster and pod status"
	@echo "  local-smoke      - Run smoke tests against running cluster"
	@echo "  local-urls       - Show access URLs"
