# Contributing

## Prerequisites

- Go 1.25+
- Docker (for the local Redash stack)

## Local dev environment

`make dev-setup` boots a complete local Redash (server, scheduler, worker, Redis,
Postgres) plus seeded sample PostgreSQL and MySQL data sources, creates an admin
user, and writes a `config.yaml` pointing at it:

```bash
make dev-setup
make run         # build and start the proxy against the local stack

# connect with psql or mysql (password is in the printed banner / your config.yaml)
psql -h 127.0.0.1 -p 15432 -U redash-wire -d "Sample PostgreSQL"
mysql -h 127.0.0.1 -P 13306 -u redash-wire -p -D "Sample MySQL"
```

Without a local `mysql` client, the sample MySQL container has one:
`docker compose exec sample-mysql mysql -h host.docker.internal -P 13306 -u redash-wire -p`.

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

`macos/RedashWire/Core/` holds the parts that own no process and no window:
the models the JSON contract decodes into, and `ProxyTracker`, which turns
the daemon's log stream and exit codes into the state the menu shows. The
SwiftPM package in `macos/Package.swift` compiles that folder on its own and
runs the tests in `macos/Tests/`:

```bash
make macos-test  # swift test --package-path macos; Command Line Tools suffice
```

Keep anything that needs AppKit, a `Process`, or SwiftUI out of `Core/`, or
the package stops building. That constraint is the point: it is what makes
the state machine testable without a running proxy.

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

```bash
scripts/release.sh 0.4.0
```

That starts the Release workflow and follows it to the end. The workflow bumps
`MARKETING_VERSION` on main, cuts an annotated tag, runs
[GoReleaser](https://goreleaser.com) for the Linux and macOS binaries (amd64 +
arm64) and the checksums, then builds `RedashWire.app` and attaches it to the
release. Running the workflow from the Actions tab does the same thing; the
script only adds the checks worth making before a runner starts, and saves you
watching the tab.

It releases whatever is on `origin/main`, so push first.

Pushing a tag by hand still works:

```bash
git tag -a v0.4.0 -m v0.4.0
git push origin v0.4.0
```

That path skips the `MARKETING_VERSION` bump, so make that commit yourself
first. The tag is created inside the release workflow rather than by a separate
one that triggers it, because a tag pushed with `GITHUB_TOKEN` does not start
another workflow run: a "prepare release" workflow would leave a tag sitting
there with nothing building it.

If a run fails after the tag exists, re-run the failed job rather than releasing
again. Starting over means `gh release delete v0.4.0 --cleanup-tag` first, since
GoReleaser will not publish over an existing release.

`install.sh` downloads from these releases and depends on the archive naming in
`.goreleaser.yaml`; change them together.
