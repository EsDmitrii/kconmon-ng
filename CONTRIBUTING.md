# Contributing to kconmon-ng

Thanks for your interest in contributing. This document covers how to build, test, and submit changes.

## Prerequisites

- Go 1.26 or newer
- Docker (for building images and running E2E tests)
- Helm 3.14+
- Minikube (for local E2E testing)

## Building

```bash
# Build both binaries
go build -o bin/kconmon-ng-agent ./cmd/agent
go build -o bin/kconmon-ng-controller ./cmd/controller
```

## Testing

```bash
# Run unit tests
go test ./...

# Run tests with race detector and coverage
go test -race -coverprofile=coverage.txt -covermode=atomic ./...

# Run E2E tests (requires a cluster with kconmon-ng installed — see hack/README.md)
go test -tags=e2e -v ./e2e/...
```

## Local E2E Testing

See [hack/README.md](hack/README.md) for a full guide on running kconmon-ng locally with Minikube, Prometheus, and Grafana — including image builds, dashboard imports, and troubleshooting.

Quick version:

```bash
./hack/local-test.sh up      # everything in one command
./hack/local-test.sh smoke   # re-run smoke tests
./hack/local-test.sh down    # tear down
```

## Linting

```bash
golangci-lint run
```

## Pre-commit Hooks

The repository ships with a `.pre-commit-config.yaml` that runs formatting, linting, and hygiene checks before every commit. Install once, run forever.

**Install pre-commit and hook dependencies:**

```bash
# Install pre-commit (requires Python)
pip install pre-commit   # or: brew install pre-commit

# Install golangci-lint at the version CI runs (the one `make lint` reads from ci.yaml)
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin $(make -s print-golangci-lint-version)

# Register the hooks in your local clone
pre-commit install
```

**Running manually:**

```bash
# Run all hooks against staged files
pre-commit run

# Run all hooks against every file in the repo
pre-commit run --all-files
```

Hooks that run on every commit:

| Hook | What it does |
|---|---|
| `trailing-whitespace` | Strips trailing whitespace |
| `end-of-file-fixer` | Ensures files end with a newline |
| `check-merge-conflict` | Blocks accidental merge conflict markers |
| `check-added-large-files` | Rejects files > 1 MB |
| `detect-private-key` | Blocks accidental secret commits |
| `goimports` | Formats Go code and organises imports |
| `golangci-lint --fix` | Runs all configured linters, auto-fixes where possible |
| `shellcheck` | Lints shell scripts in `hack/` |
| `yamllint` | Lints YAML files (workflows, Helm values, configs) |

CI re-runs lint even if you skip pre-commit locally, so code that reaches a PR will always be checked.

## Helm

```bash
# Lint the chart
helm lint charts/kconmon-ng

# Lint with custom values
helm lint charts/kconmon-ng -f charts/kconmon-ng/ci/minimal-values.yaml
```

## Documentation

The docs site at <https://esdmitrii.github.io/kconmon-ng/> is built with MkDocs Material from `docs/` and `mkdocs.yml`; the toolchain is pinned in `requirements-docs.txt`.

```bash
# Install the toolchain (a venv is a good idea) and preview at http://127.0.0.1:8000 with live reload
pip install -r requirements-docs.txt && mkdocs serve

# The PR gate: --strict turns warnings (broken nav entries, bad snippet paths, dead links) into failures
mkdocs build --strict
```

`make docs-serve` and `make docs-build` wrap the same two commands; the Makefile build lands outside the repo, in `/tmp/kconmon-ng-site`, so no stray `site/` shows up in the tree. CI runs the strict build on every PR that touches `docs/**`, `mkdocs.yml` or `requirements-docs.txt`, and a push to `main` deploys the site to GitHub Pages.

Link to site pages, not to `docs/*.md` paths. `RELEASE_NOTES.md` is included on the site verbatim, so a repo-relative path like `docs/metrics.md` is dead text there; write `https://esdmitrii.github.io/kconmon-ng/metrics/` instead.

## Submitting a Pull Request

1. Fork the repository and create a branch from `main`.
2. Make your changes. Ensure tests pass and lint succeeds.
3. Run the full test suite locally if possible.
4. Open a PR against `main`. Describe your changes clearly.
5. CI will run lint, tests, build, and Helm validation. E2E tests run on PRs.
6. Address any review feedback.

## Release Process

Releases are created when tags matching `v*` are pushed. The release workflow builds binaries, Docker images, and publishes the Helm chart to GHCR.
