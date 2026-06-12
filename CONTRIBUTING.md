# Contributing

## Prerequisites

- Go 1.25+
- Docker (for the local Redash stack)

## Local dev environment

`make dev-setup` boots a complete local Redash (server, scheduler, worker, Redis,
Postgres) plus a seeded sample PostgreSQL data source, creates an admin user, and
writes a `config.yaml` pointing at it:

```bash
make dev-setup
make run         # build and start the proxy against the local stack

# connect with psql (password is in the printed banner / your config.yaml)
psql -h 127.0.0.1 -p 15432 -U redash-wire -d "Sample PostgreSQL"
```

`make dev-down` tears the stack down; `make dev-logs` tails it.

## Building and testing

```bash
make build       # binary lands in bin/redash-wire
make test        # unit + in-process integration tests (no Docker needed)
make test-race   # with the race detector
make lint        # golangci-lint (v2 config)
make vet
```

The integration tests in `internal/integration` drive real `pgx` and
`go-sql-driver/mysql` clients against in-process servers, so the whole suite runs
in CI without external services.

## README demo GIFs

`dev/demo.gif` and `dev/wizard.gif` are recorded with
[VHS](https://github.com/charmbracelet/vhs). With the dev stack running and a
fresh binary built:

```bash
brew install vhs
vhs dev/demo.tape   # rewrites dev/demo.gif

# wizard.tape types a real API key into the form; substitute it at record time
# so the key never lands in the repo:
KEY=$(grep -o 'api_key: ".*"' config.yaml | cut -d'"' -f2)
sed "s/__REDASH_API_KEY__/$KEY/" dev/wizard.tape > /tmp/wizard.tape
vhs /tmp/wizard.tape   # rewrites dev/wizard.gif
```

## Releasing (maintainers)

Tag a version and push it; GitHub Actions runs
[GoReleaser](https://goreleaser.com) to build binaries for Linux and macOS
(amd64 + arm64), generate checksums, and publish a GitHub release:

```bash
git tag v0.1.0
git push origin v0.1.0
```

`install.sh` downloads from these releases and depends on the archive naming in
`.goreleaser.yaml`; change them together.
