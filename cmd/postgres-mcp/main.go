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
	"log"
	"os"

	"postgres-mcp/internal"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("postgres-mcp: %v", err)
	}
}

// run reads the process's -connection flags (see parseArgs, flags.go) and
// hands them off to internal.Serve, which owns all other behavior
// (connection defaults, resource limits, tool registration, and running
// the MCP stdio server). Kept separate from main so it can be exercised
// directly in tests without touching the real process's stdin/stdout.
func run(args []string) error {
	values, err := parseArgs(args)
	if err != nil {
		return err
	}

	return internal.Serve(values, os.Stdin, os.Stdout)
}
