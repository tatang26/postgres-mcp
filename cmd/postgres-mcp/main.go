// Command postgres-mcp is a minimal MCP server exposing a small set of
// read/write tools against one or more Postgres databases, described by
// one or more repeated -connection flags, each a single JSON object, e.g.:
//
//	postgres-mcp \
//	  -connection='{"uri": "postgres://postgres@localhost:5432/byod_development", "access_mode": "unrestricted"}' \
//	  -connection='{"uri": "postgres://user:pass@prodcf.example.com:5432/byod"}'
//
// "access_mode" defaults to "restricted" (read-only; only SELECT-like
// statements are permitted) if omitted. "unrestricted" permits any
// statement, including writes and DDL. "name" may be set explicitly to
// identify a connection in tool calls; if omitted, a name is derived from
// the URI (as "host/dbname").
//
// Every tool call must specify which connection to use via a "connection"
// argument; call the list_connections tool to discover what's available.
//
// The server communicates over stdio using newline-delimited JSON-RPC 2.0
// (the MCP stdio transport).
package main

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"log"
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("postgres-mcp: %v", err)
	}
}

// run contains all of the process's actual behavior, parameterized over
// its external inputs (CLI args, stdio) so it can be exercised directly in
// tests without touching real global state (the process's real
// stdin/stdout/stderr). Connections come from repeated -connection flags
// (see parseArgs); the POSTGRES_MCP_* limit env vars are still read via
// limitsFromEnv.
func run(args []string) error {
	values, err := parseArgs(args)
	if err != nil {
		return err
	}

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

	return srv.Run(ctx, os.Stdin, os.Stdout)
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

// registerTools wires each database.Connection method up as an MCP tool
// with its JSON schema and argument parsing, plus a list_connections
// discovery tool. Every data-access tool requires a "connection" argument
// naming which registered connection (see list_connections) to run
// against.
func registerTools(srv *mcpserver.Server, dbSrv database.Server) {
	srv.AddTool(&mcpserver.Tool{
		Name:        "list_connections",
		Description: "List the named Postgres connections available to the other tools, along with each one's access mode (restricted = read-only, unrestricted = read/write).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcpserver.CallToolResult, error) {
			text, err := marshalConnectionList(dbSrv)
			if err != nil {
				return nil, err
			}
			return mcpserver.TextResult(text), nil
		},
	})

	srv.AddTool(&mcpserver.Tool{
		Name:        "execute_sql",
		Description: "Execute a SQL statement against a named Postgres connection (see list_connections) and return the results as JSON.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{
					"type":        "string",
					"description": "Name of the connection to run against, as returned by list_connections.",
				},
				"sql": map[string]any{
					"type":        "string",
					"description": "SQL statement to execute.",
				},
			},
			"required": []string{"connection", "sql"},
		},
		Handler: func(ctx context.Context, args jsontext.Value) (*mcpserver.CallToolResult, error) {
			var params struct {
				Connection string `json:"connection"`
				SQL        string `json:"sql"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			conn, err := dbSrv.Get(ctx, params.Connection)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			text, err := conn.ExecuteQuery(ctx, params.SQL)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			return mcpserver.TextResult(text), nil
		},
	})

	srv.AddTool(&mcpserver.Tool{
		Name:        "explain_query",
		Description: "Return the Postgres query plan (EXPLAIN, in JSON format) for a SQL statement against a named connection (see list_connections). Set analyze=true to also execute the statement and gather real timing/row-count statistics (EXPLAIN ANALYZE).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{
					"type":        "string",
					"description": "Name of the connection to run against, as returned by list_connections.",
				},
				"sql": map[string]any{
					"type":        "string",
					"description": "SQL statement to explain.",
				},
				"analyze": map[string]any{
					"type":        "boolean",
					"description": "If true, actually execute the statement to gather real execution statistics (EXPLAIN ANALYZE). Defaults to false.",
				},
			},
			"required": []string{"connection", "sql"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcpserver.CallToolResult, error) {
			var params struct {
				Connection string `json:"connection"`
				SQL        string `json:"sql"`
				Analyze    bool   `json:"analyze"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			conn, err := dbSrv.Get(ctx, params.Connection)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			text, err := conn.ExplainQuery(ctx, params.SQL, params.Analyze)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			return mcpserver.TextResult(text), nil
		},
	})

	srv.AddTool(&mcpserver.Tool{
		Name:        "list_schemas",
		Description: "List all schemas in a named Postgres connection (see list_connections).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{
					"type":        "string",
					"description": "Name of the connection to run against, as returned by list_connections.",
				},
			},
			"required": []string{"connection"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcpserver.CallToolResult, error) {
			var params struct {
				Connection string `json:"connection"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			conn, err := dbSrv.Get(ctx, params.Connection)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			text, err := conn.ListSchemas(ctx)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			return mcpserver.TextResult(text), nil
		},
	})

	srv.AddTool(&mcpserver.Tool{
		Name:        "list_objects",
		Description: "List objects (tables, views, sequences, or extensions) within a schema, on a named connection (see list_connections).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{
					"type":        "string",
					"description": "Name of the connection to run against, as returned by list_connections.",
				},
				"schema_name": map[string]any{
					"type":        "string",
					"description": "Schema to list objects from.",
				},
				"object_type": map[string]any{
					"type":        "string",
					"enum":        []string{"table", "view", "sequence", "extension"},
					"description": "Type of object to list. Defaults to \"table\".",
				},
			},
			"required": []string{"connection", "schema_name"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (*mcpserver.CallToolResult, error) {
			var params struct {
				Connection string `json:"connection"`
				SchemaName string `json:"schema_name"`
				ObjectType string `json:"object_type"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			conn, err := dbSrv.Get(ctx, params.Connection)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			text, err := conn.ListObjects(ctx, params.SchemaName, params.ObjectType)
			if err != nil {
				return mcpserver.ErrorResult(err), nil
			}
			return mcpserver.TextResult(text), nil
		},
	})
}

func marshalConnectionList(dbSrv database.Server) (string, error) {
	type connOut struct {
		Name       string `json:"name"`
		AccessMode string `json:"access_mode"`
	}

	infos := dbSrv.List()
	out := make([]connOut, 0, len(infos))
	for _, info := range infos {
		out = append(out, connOut{Name: info.Name, AccessMode: info.AccessMode.String()})
	}

	data, err := json.MarshalIndent(map[string]any{"connections": out}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal connections: %w", err)
	}
	return string(data), nil
}
