# Contributing to boxy

## Getting started

1. Fork the repository and clone your fork.
2. Install dependencies: Go 1.22+, Docker, `kind`, `kubectl`, `helm`.
3. Run unit tests: `make test`
4. Run lint: `make lint`

## Project structure

```
cmd/               — three binaries: boxy-router, boxy-operator, boxy-controller
internal/          — shared packages (nsjail adapter, API types, controller client)
deploy/helm/boxy/  — Helm chart
test/e2e/          — Go + shell E2E tests (require a running cluster)
local/             — helper scripts for local development
```

## Making changes

- Open an issue to discuss significant changes before starting.
- Keep PRs focused on a single concern.
- Add or update unit tests for any changed behaviour.
- Run `make test lint` before opening a PR.
- Follow the existing code style (no comments unless the WHY is non-obvious).

## Running E2E tests

```bash
make e2e          # spins up a kind cluster, deploys boxy, runs the full suite
make e2e-go       # Go tests only (requires BOXY_E2E_BASE_URL and BOXY_E2E_ROUTER_TOKEN)
make e2e-scripts  # shell scripts only (requires BASE_URL, ROUTER_TOKEN, NAMESPACE)
```

## Security issues

Please do **not** open a public GitHub issue for security vulnerabilities.
See [SECURITY.md](SECURITY.md) for the disclosure process.
