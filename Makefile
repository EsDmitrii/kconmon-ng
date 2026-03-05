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

.PHONY: all build build-agent build-controller test test-race test-cover lint fmt proto clean help \
	local-up local-down local-status local-smoke local-urls

all: lint test build

## Build

build: build-agent build-controller

build-agent:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/kconmon-ng-agent ./cmd/agent

build-controller:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/kconmon-ng-controller ./cmd/controller

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

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .
	goimports -w .

## Proto

proto:
	buf generate api/proto

## Helm

helm-lint:
	helm lint charts/kconmon-ng
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/default-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/full-values.yaml
	helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/minimal-values.yaml

helm-template:
	helm template kconmon-ng charts/kconmon-ng

helm-package:
	helm package charts/kconmon-ng -d dist/

## Docker

docker-build:
	docker build --target agent -t $(PROJECT_NAME)-agent:$(VERSION) .
	docker build --target controller -t $(PROJECT_NAME)-controller:$(VERSION) .

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
	@echo "  build            - Build agent and controller binaries"
	@echo "  test             - Run unit tests"
	@echo "  test-race        - Run tests with race detector"
	@echo "  test-cover       - Run tests with coverage"
	@echo "  test-fuzz        - Run fuzz tests"
	@echo "  lint             - Run golangci-lint"
	@echo "  fmt              - Format code"
	@echo "  proto            - Generate protobuf code"
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
