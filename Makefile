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

# Regenerates web/src/lib/api-types.ts from the hand-authored
# docs/console-api.yaml. Unlike proto:/sqlc: above there is no `go run` to pin
# the tool with -- the generator is openapi-typescript, so this needs NODE and
# an installed web/ tree (`cd web && npm ci`). The version is pinned by
# web/package-lock.json instead, which is the same guarantee by another means.
# CI runs this and fails on a diff, so a spec edit without a regenerate cannot
# land; api-types.ts is committed, exactly like api/proto/*.pb.go and
# internal/console/store/gen/.

openapi:
	cd web && npm run gen:api

## Helm

helm-lint:
	helm lint charts/kconmon-ng
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/default-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/full-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/minimal-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/console-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/console-db-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/console-auth-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/console-targets-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/console-mtr-values.yaml

# full-values.yaml is templated as well as linted: `helm lint` validates values
# against values.schema.json but does not prove every template renders, and the
# profile that turns the most knobs on is exactly the one worth rendering.
helm-template:
	helm template kconmon-ng charts/kconmon-ng
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/full-values.yaml
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/console-values.yaml
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/console-db-values.yaml
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/console-auth-values.yaml
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/console-targets-values.yaml
	helm template kconmon-ng charts/kconmon-ng -f charts/kconmon-ng/ci/console-mtr-values.yaml

helm-package:
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
