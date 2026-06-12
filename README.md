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
it's talking to the Redash REST API.

Connect using the name of a Redash data source as the database name (on MySQL,
pick one with `USE <name>`). Run a query and the proxy hands it to the Redash
API, waits for the job to finish, and gives you back the rows as a normal result
set. Schema browsing and the usual connection chatter are answered locally, so
GUI clients can list tables without complaining.

## Install

On macOS or Linux, one line:

```bash
curl -fsSL https://raw.githubusercontent.com/lhpalacio/redash-wire/main/install.sh | sh
```

The script detects your OS and architecture, downloads the latest release, verifies the
checksum, and installs to `/usr/local/bin` (set `BIN_DIR` or `VERSION` to
override). Prebuilt binaries for Linux and macOS (amd64 + arm64) are on the
[releases page](https://github.com/lhpalacio/redash-wire/releases); Windows is
not supported.

Run `redash-wire`. With no config present, the first run drops you into a setup
wizard: it asks for your Redash URL and API key, tests the connection, and
writes a config with one profile. It also enables a listener for each data
source type it finds (Postgres, MySQL) and leaves the rest commented out, ready
to turn on later.

Config is looked up in order: `-config <path>`, then `./config.yaml`, then
`~/.redash-wire/config.yaml`. To run against more than one Redash, add profiles
by hand (see below).

![first run: the setup wizard connects to Redash and writes the config](dev/wizard.gif)

## Configuration

The wizard writes a single profile to `~/.redash-wire/config.yaml`. To talk to
more than one Redash, add profiles to that file by hand and pick one with the
`-profile` flag. A config with two profiles looks like this
([`config.example.yaml`](config.example.yaml)):

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
```

Unknown keys fail at startup, so typos don't get silently ignored.

Traffic between client and proxy is plaintext (no TLS), and any client that
authenticates can reach everything the configured API key can. Keep the
listeners on localhost unless you trust the network.

CLI flags: `-config <path>`, `-profile <name>`, `-debug`, `-version`.

## Supported SQL & limitations

It runs read queries against any PostgreSQL- or MySQL-compatible Redash data
source. A few things worth knowing:

- One statement per request. Multi-statement batches (`BEGIN; ...; COMMIT` in a
  single message) get rejected outright rather than half-executed.
- Writes are forwarded, but Redash never reports how many rows changed, so
  neither can the proxy. Postgres clients get a `NOTICE` to that effect. Add
  `RETURNING` when you need a count.
- Introspection is best-effort. The common `pg_catalog` / `information_schema`
  queries (and MySQL's `SHOW` / `information_schema`) are answered from the
  cached schema; anything exotic may come back partial. Every column type
  reports as `text`, since Redash's schema endpoint doesn't expose real types.
- Numbers stay exact. Integers and decimals never round-trip through `float64`;
  a value too big for the native type comes back as text rather than getting
  mangled.
- The extended/prepared-statement protocol isn't supported. Use the simple
  query protocol.

## Contributing

Development setup, tests, and the release process are documented in
[CONTRIBUTING.md](CONTRIBUTING.md).
