# redash-wire

**Query [Redash](https://redash.io/) from any PostgreSQL or MySQL client.**

`redash-wire` lets you connect apps like TablePlus or DBeaver straight to
Redash and query your data sources as if they were normal databases. No Redash
UI, no pasting SQL into the query editor.

![redash-wire starting up, then a psql session querying a Redash data source](dev/demo.gif)

## How it works

```
psql / TablePlus / DBeaver  ──▶  redash-wire  ──▶  Redash  ──▶  your data sources
```

To your client it looks like a regular Postgres or MySQL server. Underneath,
it talks to the Redash REST API. Connect with the name of a Redash data source
as the database name (on MySQL, pick one with `USE <name>`). The proxy runs
each query through Redash and returns the rows as a normal result set.

## Install

On macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/lhpalacio/redash-wire/main/install.sh | sh
```

The script installs the latest release to `/usr/local/bin` (set `BIN_DIR` or
`VERSION` to override). Binaries for Linux and macOS, amd64 and arm64, are on
the [releases page](https://github.com/lhpalacio/redash-wire/releases).
Windows is not supported.

Then run `redash-wire`. The first run asks for your Redash URL and API key,
tests the connection, and writes `~/.redash-wire/config.yaml` with one profile.

![first run: the setup wizard connects to Redash and writes the config](dev/wizard.gif)

## Configuration

The proxy reads `-config <path>`, then `./config.yaml`, then
`~/.redash-wire/config.yaml`. To talk to more than one Redash, add profiles
and pick one with `-profile` ([`config.example.yaml`](config.example.yaml)):

```yaml
# Top-level keys are defaults for every profile; a profile can override them.
# At least one listener must be configured.
postgres_listen_addr: "127.0.0.1:15432"  # omit to disable; keep on loopback unless you mean to expose it
mysql_listen_addr: "127.0.0.1:13306"     # omit to disable

# Proxy login. Defaults to redash-wire / supersecret; change them for anything
# beyond loopback.
username: "redash-wire"
password: "supersecret"

poll_interval: "500ms"  # how often to poll Redash for query completion
poll_timeout: "120s"    # give up on a query after this long

read_only: false  # true refuses every statement that is not a read (see below)

# Profile used when the -profile flag is omitted.
default_profile: integration

profiles:
  integration:
    redash_url: "https://redash.integration.example.com"
    api_key: "${REDASH_INTEGRATION_API_KEY}"  # ${ENV_VAR} expansion works for redash_url and api_key
  prod:
    redash_url: "https://redash.prod.example.com"
    api_key: "${REDASH_PROD_API_KEY}"
    postgres_listen_addr: "127.0.0.1:25432"  # per-profile override
    read_only: true                          # lock this one
```

Unknown keys fail at startup. Traffic between client and proxy is plaintext,
and any client that logs in can reach everything the API key can, so keep the
listeners on localhost unless you trust the network.

### Read-only mode

`read_only: true` makes the proxy refuse every statement that is not a read
before it reaches Redash. Set it at the top level for every profile, or on one
profile; a profile can also set `read_only: false` under a top-level `true`.
It's meant for a profile an AI agent or a script connects through, where a
stray `UPDATE` would cost more than the query is worth.

What goes through: `SELECT`, `WITH … SELECT`, `VALUES`, `TABLE`, `SHOW`,
`DESCRIBE`, and `EXPLAIN` of any of those. Everything else is refused, including
a read that carries a write: a data-modifying CTE, `SELECT … INTO`, `SELECT …
FOR UPDATE`, `EXPLAIN ANALYZE UPDATE …`. The refusal is the one a read-only
server sends, so clients already know it: SQLSTATE `25006` on PostgreSQL, error
`1290` on MySQL, with a hint naming redash-wire. The mode is reported too:
`SHOW transaction_read_only` and `SELECT @@read_only` say on, and DBeaver and
DataGrip show their read-only badge. A session can't switch it off; `SET
transaction_read_only = off` is refused rather than silently accepted.

The check is text matching, not a database permission. A function with side
effects, like `setval()` or `pg_terminate_backend()`, is not caught. When you
need a real boundary, give the Redash data source a read-only database user.

`redash-wire -read-only` forces the mode for one run. It can only tighten: a
profile with `read_only: true` stays read-only without the flag.

## CLI

`redash-wire` with no arguments starts the proxy. Flags: `-config <path>`,
`-profile <name>`, `-debug`, `-version`, `-log-format <text|json>`,
`-exit-on-stdin-eof` (quit when stdin closes, for supervisors),
`-wait-for-redash` (bind the listeners first, and keep retrying an unreachable
Redash instead of exiting 1), and `-read-only` (refuse writes for this run,
whatever the profile says).

The proxy checks Redash every 10 seconds, so a data source added while it runs
shows up without a restart. When Redash stops answering, the proxy refuses new
sessions and answers queries on open ones with the reason, then serves again
once Redash is back. `kill -USR1 $(pgrep redash-wire)` forces a check.

Subcommands answer a question and exit, for scripts and supervisors:

```bash
redash-wire config [-json] [-show-secrets] [-config <path>]
redash-wire datasources [-json] [-config <path>] [-profile <name>]
redash-wire init -url <url> [-profile <name>] [-username <u>] [-password <p>] [-read-only] [-config <path>] [-json]
redash-wire help
```

- `config` shows the resolved configuration for every profile, with the API key
  hidden unless you pass `-show-secrets`. A missing config file reports as
  `"exists": false` with exit 0; every other command exits non-zero with
  `not_configured`.
- `datasources` lists one profile's data sources as `id`, `name`, `type`, and
  `wire`. An empty `wire` means the proxy won't serve that source.
- `init` is the setup wizard driven by flags. It reads the API key from stdin
  (`pbpaste | redash-wire init -url https://redash.example.com`), so the key
  stays out of `ps` and shell history. It writes `~/.redash-wire/config.yaml`
  unless `-config` says otherwise, and won't overwrite an existing file.
  `-read-only` writes `read_only: true` on the new profile.

Results go to stdout and logs to stderr. Exit 0 on success, 2 for a usage
mistake, 1 for everything else. With `-json` an error prints as
`{"error":{"code":"...","message":"..."}}`. The codes are stable, so branch on
the code, not the message:

- `usage`: a required flag is missing or malformed, or the API key wasn't piped in.
- `not_configured`: no config file at any of the lookup paths.
- `invalid_config`: bad YAML, an unknown key, a `default_profile` that doesn't
  exist, or a profile that fails validation.
- `profile_not_found`: `-profile` names a profile the config doesn't have.
- `connection_failed`: Redash didn't answer, or answered with an unrelated
  error such as a 500.
- `authentication_failed`: Redash rejected the key, or the URL isn't Redash.
  Redash answers a bad key with a 404, so both look alike; the message says which.
- `config_exists`: `init` found a config already in place and left it alone.
- `io_error`: a file couldn't be read or written.

## macOS app

A menu bar app runs the proxy for you: no terminal, no Dock icon. It needs
macOS 13 or newer and bundles its own copy of `redash-wire`. The first run asks
for your Redash URL and API key, and whether to start read-only. From the menu
you can:

- Pick a profile and start or stop the proxy. `default_profile` starts at launch.
- Lock the selected profile to read-only. The choice is remembered per profile
  and the proxy restarts with it; a profile whose config says `read_only: true`
  shows as locked and can't be unlocked from the menu. The status line and the
  profile list say which profiles are read-only.
- Browse the proxy's data sources and copy a `psql` or `mysql` command or a
  connection URI for each, or open one in an app that handles `postgresql://`
  links, like TablePlus.
- Copy the proxy username and password. A copied password leaves the clipboard
  after 60 seconds.
- See whether Redash is answering. A dropped VPN shows within about fifteen
  seconds.
- Follow the proxy's log stream, filtered by level and text.
- Turn on launch at login, check for a new release, edit `config.yaml`, and
  reload it.

The status dot and the menu bar icon show the proxy's state: grey stopped,
yellow starting, green running, amber cut off from Redash, red failed.

Build it with `make macos`, or `make macos-run` to build and open it. That
needs full Xcode 16 or newer, not just the Command Line Tools. The app is
signed ad-hoc and not notarized, so Gatekeeper blocks a copy that arrived over
the network: right-click it and choose Open, or run
`xattr -dr com.apple.quarantine /path/to/RedashWire.app`. Build from a checkout
you trust before handing the app a Redash API key.

## Supported SQL & limitations

The proxy runs read queries against any PostgreSQL- or MySQL-compatible Redash
data source.

- One statement per request. The proxy rejects multi-statement batches outright.
- Writes go through unless the profile is read-only (see [Read-only
  mode](#read-only-mode)). Redash doesn't report rows changed, so neither does
  the proxy. Add `RETURNING` when you need a count.
- On a MySQL data source, writes only stick when the data source's
  **Autocommit** option is on in Redash. Redash opens a fresh connection per
  query and closes it without `COMMIT`, so with autocommit off the change is
  rolled back and the proxy has no way to tell.
- Introspection is best-effort. The proxy answers common `pg_catalog`,
  `information_schema`, and MySQL `SHOW` queries from the schema Redash
  reports: table and column names and types, no keys, indexes, defaults or
  nullability. On the Postgres wire every result column type reports as `text`.
- Numbers stay exact. A value too big for the native type comes back as text.
- No extended/prepared-statement protocol. Use the simple query protocol.
