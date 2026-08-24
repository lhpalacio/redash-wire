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

## macOS app

The menu bar app is a SwiftUI target; its sources live in `macos/RedashWire/`
and the Xcode project next to them in `macos/RedashWire.xcodeproj`:

```bash
make macos       # builds build/RedashWire.app
make macos-run   # builds it and opens it
```

Both run `scripts/build-app.sh`, which builds a universal `redash-wire`, calls
`xcodebuild`, embeds the binary in the bundle, and signs the nested binary and
then the bundle. It needs full Xcode: `xcodebuild` doesn't ship with the Command
Line Tools.

The target uses a file system synchronized group, so it compiles whatever Swift
files are in `macos/RedashWire/`. Adding a file needs no project edit, which
also keeps `project.pbxproj` out of most diffs. Opening the project needs
Xcode 16 or newer; older versions can't read a synchronized group.

The app runs the copy of `redash-wire` inside its bundle. To point it at a
local build instead, set `REDASH_WIRE_BINARY` and launch the executable directly
so it inherits the variable:

```bash
make build
REDASH_WIRE_BINARY=$PWD/bin/redash-wire build/RedashWire.app/Contents/MacOS/RedashWire
```

## JSON output and golden files

The macOS menu bar app decodes the `-json` payloads of `config`, `datasources`,
and `init`, along with the error envelope they share, so their shape is a
contract. `cmd/redash-wire/json_test.go` pins each one against a golden file in
`cmd/redash-wire/testdata/`:

```bash
go test ./cmd/redash-wire            # compare the payloads against the goldens
go test ./cmd/redash-wire -update    # rewrite the goldens from the current code
```

A golden diff means a consumer breaks, so update the files only as part of a
change you meant to make, and read the diff before you commit it. The error
codes in `cmd/redash-wire/cli.go` are part of the same contract: adding a code
is safe, renaming or repurposing one is not.

## README demo GIFs

We record `dev/demo.gif` and `dev/wizard.gif` with
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
