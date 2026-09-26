# Contributing to kconmon-ng

Thanks for your interest in contributing. This document covers how to build, test, and submit changes.

## Prerequisites

- Go 1.26 or newer
- Docker (for building images and running E2E tests)
- Helm 3.14+
- Minikube (for local E2E testing)

## Building

```bash
# Build the agent, controller and console binaries into bin/
make build
```

The console embeds the web UI from `internal/console/ui/dist`. A plain
`cd web && npm run build` resets `dist/index.html` to the tracked placeholder
afterwards and prints one line saying so. To embed a working UI in a local
console binary, build with `KCONMON_EMBED_BUILD=1 npm run build` and then
`make build-console`; git then shows `index.html` as modified, so do not
commit it. `npm test`, vitest and `npm run dev` leave `dist` alone.

## Testing

```bash
# Run unit tests
go test ./...

# Run tests with race detector and coverage
go test -race -coverprofile=coverage.txt -covermode=atomic ./...

# Run E2E tests (requires a cluster with kconmon-ng installed; see hack/README.md)
go test -tags=e2e -v ./e2e/...

# Fuzz the UDP packet parser and the external-check allowlist, one after the other, 30s each
make test-fuzz              # FUZZTIME=2m make test-fuzz for longer runs
```

## Local E2E Testing

See [hack/README.md](hack/README.md) for a full guide on running kconmon-ng locally with Minikube, Prometheus, and Grafana, including image builds, dashboard imports, and troubleshooting.

Quick version (the script also needs `openssl`, `python3`, `curl` and `lsof` on `PATH`):

```bash
./hack/local-test.sh up      # everything in one command
./hack/local-test.sh smoke   # re-run smoke tests
./hack/local-test.sh down    # tear down
```

## Linting

```bash
make lint
```

`make lint` runs the golangci-lint version CI pins; `.golangci.yml` turns on the `e2e` and `integration` build tags, so the E2E suite and the store, scheduler and cache integration tests are linted too, and a plain `golangci-lint run ./...` of your own picks them up.

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

The docs site at <https://esdmitrii.github.io/kconmon-ng/> is built with MkDocs Material from `docs/` and `mkdocs.yml`; the toolchain is pinned in `requirements-docs.txt`, and CI installs it from `.github/requirements-docs.lock` with `pip install --require-hashes`, which pins every transitive package by hash.

```bash
# Install the toolchain (a venv is a good idea) and preview at http://127.0.0.1:8000 with live reload
pip install --require-hashes -r .github/requirements-docs.lock && mkdocs serve

# The PR gate: --strict turns warnings (broken nav entries, bad snippet paths, dead links) into failures
mkdocs build --strict
```

`make docs-serve` and `make docs-build` wrap the same two commands; the Makefile build lands outside the repo, in `/tmp/kconmon-ng-site`, so no stray `site/` shows up in the tree. CI runs the strict build on every PR that touches `docs/**`, `mkdocs.yml`, `requirements-docs.txt`, the lock, the docs workflow, or `RELEASE_NOTES.md` and `charts/kconmon-ng/values.yaml`, which the site embeds, and a push to `main` deploys the site to GitHub Pages. Whoever edits `requirements-docs.txt` regenerates the lock with the `uv pip compile` command in its header; the docs job fails when a pin in `requirements-docs.txt` is missing from the lock.

Link to site pages, not to `docs/*.md` paths. `RELEASE_NOTES.md` is included on the site verbatim, so a repo-relative path like `docs/metrics.md` is dead text there; write `https://esdmitrii.github.io/kconmon-ng/metrics/` instead.

## Submitting a Pull Request

1. Fork the repository and create a branch from `main`.
2. Make your changes. Ensure tests pass and lint succeeds.
3. Run the full test suite locally if possible.
4. Open a PR against `main`. Describe your changes clearly.
5. CI runs lint, tests, build and Helm validation on the PR, plus a goreleaser snapshot (every binary, package and thin-Dockerfile image the release builds, without publishing or signing) and builds of the root `Dockerfile` and `Dockerfile.console`, so a broken Dockerfile or `.goreleaser.yaml` fails the PR rather than the tag. E2E does not run on PRs: it runs from the release workflow on a `v*` tag, or by hand through the E2E workflow's `workflow_dispatch`.
6. Address any review feedback.

## Release Process

Releases are created when tags matching `v*` are pushed. The release workflow runs CI, then builds binaries and images, pushes the images by digest and creates a draft GitHub release. E2E then runs on kind against those exact images: every leg, including one with `networkPolicy.enabled=true` that kindnet enforces, which asserts refused flows (a port the ingress rules do not name, an agent egress no rule names) as well as allowed ones and runs the chart's `helm test` under those policies, and any skipped test or a `-run` that matched nothing fails it. Only after E2E passes does the publish job push the Helm chart to GHCR, publish the draft release and, for the newest stable tag only, move `:latest` and the GitHub Latest badge and open the krew-index bump PR. A failed E2E leaves the versioned images with no chart, `:latest` or published release pointing at them. The publish and krew jobs run one at a time per repository, so two stable tags in flight cannot both count as newest. GitHub keeps one pending run per group: a third tag queued behind a pending publish cancels that pending one, and its publish job then needs a re-run.
