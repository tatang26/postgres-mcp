// Package internal is a minimal MCP server exposing a small set of
// read/write tools against one or more Postgres databases, described by
// one or more repeated -connection flags, each a single JSON object, e.g.:
//
//	postgres-mcp \
//	  -connection='{"name": "dev", "uri": "postgres://postgres@localhost:5432/byod_development", "access_mode": "unrestricted"}' \
//	  -connection='{"name": "prod", "uri": "postgres://user:pass@prodcf.example.com:5432/byod"}'
//
// "access_mode" defaults to "restricted" (read-only; only SELECT-like
// statements are permitted) if omitted. "unrestricted" permits any
// statement, including writes and DDL.
//
// Every tool call must specify which connection to use via a "connection"
// argument; call the list_connections tool to discover what's available.
//
// The server communicates over stdio using newline-delimited JSON-RPC 2.0
// (the MCP stdio transport).
package internal

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"postgres-mcp/internal/database"
	"postgres-mcp/internal/database/config"
	"postgres-mcp/internal/mcpserver"
)

const serverVersion = "0.1.0"

// Serve parses values (the raw JSON of each -connection flag occurrence,
// in the order given - see config.Parse) and the POSTGRES_MCP_* limit env
// vars (see limitsFromEnv), builds the database and MCP servers, and runs
// the MCP stdio server against stdin/stdout until stdin is exhausted (EOF)
// or the process receives SIGINT/SIGTERM.
func Serve(values []string, stdin io.Reader, stdout io.Writer) error {
	connections, err := config.Parse(values)
	if err != nil {
		return err
	}

	limits, err := limitsFromEnv()
	if err != nil {
		return err
	}

	if err := applyConnectionDefaults(connections, limits.QueryTimeout); err != nil {
		return err
	}

	dbSrv := database.New(
		database.RegisterConnections(connections...),
		database.WithLimits(limits),
	)
	defer dbSrv.Close()

	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	// Ensure dbSrv.Close() (and any other deferred cleanup) actually runs
	// on SIGINT/SIGTERM, instead of the process being killed out from
	// under it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx, stdin, stdout)
}

// applyConnectionDefaults rewrites each connection's URI in place via
// withDefaultSSLMode and withStatementTimeout (see dsn.go), so every
// registered connection has an explicit sslmode and a server-side
// statement_timeout matching queryTimeout. This is deliberately
// non-blocking: it never opens a network connection. Each connection is
// actually connected to lazily, on first use, by database.Server.Get -
// see internal/database for why. This is what lets the MCP server respond
// to its initial handshake immediately regardless of how many connections
// are configured or whether some of them (e.g. a production database
// behind a VPN) are currently reachable at all.
func applyConnectionDefaults(connections []config.Connection, queryTimeout time.Duration) error {
	for i, c := range connections {
		dsn, err := withDefaultSSLMode(c.URI)
		if err != nil {
			return fmt.Errorf("connection %q: %w", c.Name, err)
		}
		dsn, err = withStatementTimeout(dsn, queryTimeout)
		if err != nil {
			return fmt.Errorf("connection %q: %w", c.Name, err)
		}
		connections[i].URI = dsn
	}
	return nil
}

// limitsFromEnv builds a database.Limits from database.DefaultLimits,
// overridden by any of POSTGRES_MCP_QUERY_TIMEOUT (a Go duration string,
// e.g. "30s"), POSTGRES_MCP_MAX_ROWS, or POSTGRES_MCP_MAX_BYTES that are
// set.
func limitsFromEnv() (database.Limits, error) {
	limits := database.DefaultLimits()

	if v := os.Getenv("POSTGRES_MCP_QUERY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return limits, fmt.Errorf("invalid POSTGRES_MCP_QUERY_TIMEOUT %q: must be a positive Go duration (e.g. \"30s\")", v)
		}
		limits.QueryTimeout = d
	}

	if v := os.Getenv("POSTGRES_MCP_MAX_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return limits, fmt.Errorf("invalid POSTGRES_MCP_MAX_ROWS %q: must be a positive integer", v)
		}
		limits.MaxRows = n
	}

	if v := os.Getenv("POSTGRES_MCP_MAX_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return limits, fmt.Errorf("invalid POSTGRES_MCP_MAX_BYTES %q: must be a positive integer", v)
		}
		limits.MaxBytes = n
	}

	return limits, nil
}
