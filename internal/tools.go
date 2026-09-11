package internal

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"

	"postgres-mcp/internal/database"
	"postgres-mcp/internal/mcpserver"
)

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
