# postgres-mcp

A minimal [MCP](https://modelcontextprotocol.io) server, written in Go, that exposes one or more
PostgreSQL databases to an MCP client (Claude Code, Claude Desktop, OpenCode, …).

Each database is registered as a **named connection** with its own **access mode**:

- `restricted` (default) — read-only. Enforced by a SQL keyword guard *and* a database-level
  read-only transaction.
- `unrestricted` — any statement, including writes and DDL.

The server speaks the MCP stdio transport (newline-delimited JSON-RPC 2.0), protocol version
`2024-11-05`. Dependencies: the Go standard library plus [`lib/pq`](https://github.com/lib/pq).

## Tools

| Tool | Arguments | Notes |
| --- | --- | --- |
| `list_connections` | — | Names + access modes of registered connections. Never returns credentials. |
| `execute_sql` | `connection`, `sql` | Runs a statement, returns rows as JSON. Subject to the access mode. |
| `explain_query` | `connection`, `sql`, `analyze?` | `EXPLAIN (FORMAT JSON)`; `analyze=true` adds `ANALYZE` (which executes the statement). |
| `list_schemas` | `connection` | Always permitted regardless of access mode. |
| `list_objects` | `connection`, `schema_name`, `object_type?` | `object_type` ∈ `table` (default), `view`, `sequence`, `extension`. Always permitted. |

Every data-access tool requires a `connection` argument. Clients should call `list_connections`
first to discover what is available.

## Build

Requires Go 1.27+ (the code uses `encoding/json/v2`).

```sh
go build -o bin/postgres-mcp ./cmd/postgres-mcp
```

## Configuring connections

Connections are passed as repeated `-connection` flags, each a single JSON object:

```sh
bin/postgres-mcp \
  -connection='{"name":"local","uri":"postgresql://postgres@localhost:5432/app_development","access_mode":"unrestricted"}' \
  -connection='{"name":"prod","uri":"postgresql://user:pass@db.example.com:5432/app"}'
```

| Field | Required | Default |
| --- | --- | --- |
| `uri` | yes | — |
| `name` | no | Derived from the URI as `host/dbname`. Colliding names get a `-2`, `-3`, … suffix. |
| `access_mode` | no | `restricted`. Anything other than the exact string `unrestricted` is treated as `restricted`. |

Notes:

- Unknown JSON fields are rejected, so a typo like `"acess_mode"` fails at startup instead of
  silently leaving a connection read-write.
- Keyword/value DSNs (`host=… dbname=…`) work, but require an explicit `name`.
- Percent-encode reserved characters in URI passwords (`@` → `%40`, `,` → `%2C`, …).
- Connections are opened **lazily**, on first use, and the handshake never blocks on them. A
  database that is unreachable (e.g. behind a VPN that isn't up) only fails when a tool actually
  targets it. Each connect attempt is bounded at 10s.

### DSN defaults applied at startup

- **`sslmode`** — if unset: `prefer` for loopback hosts (`localhost`, `127.0.0.1`, `::1`, empty),
  `require` otherwise. This overrides `lib/pq`'s unconditional `require` default so local
  development works, without silently allowing a plaintext connection to a remote host.
- **`statement_timeout`** — injected via the libpq `options` parameter to match `QueryTimeout`, so
  the deadline is also enforced server-side if a client-side cancellation is lost. Skipped if you
  already set `options` yourself.

### Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `POSTGRES_MCP_QUERY_TIMEOUT` | `30s` | Per-query deadline (Go duration string). |
| `POSTGRES_MCP_MAX_ROWS` | `10000` | Row cap per result before truncation. |
| `POSTGRES_MCP_MAX_BYTES` | `1048576` | Approximate byte cap per result before truncation. |

Truncated results are marked `truncated: true` with a `truncated_reason`. Truncation only bounds
what is read and returned — the query still runs to completion server-side, so use `LIMIT` for
genuinely large tables.

Connection pools are capped at 5 open / 2 idle connections per database, which keeps the server's
footprint on a shared database small.

## Restricted mode

Three independent layers, in the order a statement hits them:

1. **Keyword guard** (`internal/sqlguard`) — statements must lead with `SELECT`, `WITH`, `EXPLAIN`,
   `SHOW`, `TABLE`, or `VALUES`, and must not contain `INSERT`, `UPDATE`, `DELETE`, `MERGE`, or
   `UPSERT` anywhere (which also catches data-modifying CTEs). Comments and string/identifier
   literals are blanked out first, so keywords inside literals don't cause false rejections.
   Rejected statements never reach the database, and the error message excludes literal values.
2. **Read-only transaction** — permitted statements run inside
   `BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` and are always rolled back. This is the primary
   guarantee: Postgres enforces it, including for write paths the text-level guard cannot see (e.g.
   side-effecting function calls like `setval` or `pg_terminate_backend`).
3. **Database grants** — whatever the role is actually allowed to do. Prefer a role with read-only
   grants for anything pointed at production.

Layer 1 is a deliberately conservative heuristic, not a SQL parser. It is defense-in-depth, and it
over-rejects on purpose: `EXPLAIN` of a write statement is refused even though a non-`ANALYZE`
`EXPLAIN` never executes anything.

`list_schemas` and `list_objects` are read-only by construction and are always allowed.

## Audit logging

One structured JSON record per executed statement goes to **stderr** (never stdout, which carries
the JSON-RPC transport), with the connection name, access mode, operation, duration, statement text
(truncated at 2000 chars), and error if any. There is no rotation or external sink; tee stderr into
your own log pipeline if you need one.

## Client configuration

Replace the binary path, connection names, and credentials with your own. Keys are the same across
clients; only the file and the surrounding shape differ.

### OpenCode

`~/.config/opencode/opencode.json` (global) or `./opencode.json` (project):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "postgres": {
      "type": "local",
      "enabled": true,
      "command": [
        "/absolute/path/to/postgres-mcp/bin/postgres-mcp",
        "-connection={\"name\":\"local\",\"uri\":\"postgresql://postgres@localhost:5432/app_development\",\"access_mode\":\"unrestricted\"}",
        "-connection={\"name\":\"prod\",\"uri\":\"postgresql://user:pass@db.example.com:5432/app\",\"access_mode\":\"restricted\"}"
      ],
      "environment": {
        "POSTGRES_MCP_QUERY_TIMEOUT": "30s",
        "POSTGRES_MCP_MAX_ROWS": "10000"
      }
    }
  }
}
```

Notes specific to OpenCode:

- The whole invocation is a single `command` array — argv entries, not a shell string, so the
  `-connection={...}` values need no outer shell quoting, only JSON escaping of their inner quotes.
- Tools are exposed prefixed with the server name (`postgres_execute_sql`, `postgres_list_connections`,
  …), which is also how you gate them via `tools` or per-agent config.
- OpenCode gives an MCP server 5s by default to return its tool list (`timeout`). This server
  answers immediately because connections are opened lazily, no matter how many are configured.

### Claude Code

Project scope, in `.mcp.json` at the repository root (checked in, shared with the team — use it
only for connections whose credentials are not secret):

```json
{
  "mcpServers": {
    "postgres": {
      "type": "stdio",
      "command": "/absolute/path/to/postgres-mcp/bin/postgres-mcp",
      "args": [
        "-connection={\"name\":\"local\",\"uri\":\"postgresql://postgres@localhost:5432/app_development\",\"access_mode\":\"unrestricted\"}",
        "-connection={\"name\":\"prod\",\"uri\":\"postgresql://user:pass@db.example.com:5432/app\",\"access_mode\":\"restricted\"}"
      ],
      "env": {}
    }
  }
}
```

User scope (available in every project) uses the same object under `mcpServers` in `~/.claude.json`,
or add it from the CLI:

```sh
claude mcp add postgres --scope user -- \
  /absolute/path/to/postgres-mcp/bin/postgres-mcp \
  '-connection={"name":"prod","uri":"postgresql://user:pass@db.example.com:5432/app","access_mode":"restricted"}'
```

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS
(`%APPDATA%\Claude\claude_desktop_config.json` on Windows) — same `mcpServers` shape as above.
Restart the app after editing.

## Tests

```sh
go test ./...
```

Unit tests (flag parsing, config parsing, the SQL guard, the MCP protocol layer) need nothing
external. Integration tests want a local Postgres at
`postgres://postgres@localhost:5432/postgres?sslmode=disable`, overridable via
`PGTOOLS_TEST_DATABASE_URI`, and **skip** themselves if none is reachable.

## Security notes

- Credentials live in plaintext in the client config file, which is also world-readable unless you
  restrict it. Prefer per-user database roles with least-privilege grants, and `chmod 600` any
  config that contains passwords.
- `access_mode` is a guardrail against accidents, not a security boundary. Enforce read-only access
  with database grants too; that is the only layer an MCP client cannot influence.
- Anything registered as `unrestricted` can be dropped, truncated, or updated by the model. Keep
  production connections `restricted`.
