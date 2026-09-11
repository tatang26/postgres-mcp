package internal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"postgres-mcp/internal/database"
	"postgres-mcp/internal/database/config"
	"postgres-mcp/internal/mcpserver"
)

const defaultTestDSN = "postgres://postgres@localhost:5432/postgres?sslmode=disable"

func testDSN() string {
	if dsn := os.Getenv("PGTOOLS_TEST_DATABASE_URI"); dsn != "" {
		return dsn
	}
	return defaultTestDSN
}

// newTestServer builds a database.Server with a single connection named
// "test", pointed at a real local Postgres instance. The connection is
// registered (not yet opened - Server connects lazily on first Get); the
// test is skipped upfront if nothing is listening.
func newTestServer(t *testing.T, mode config.AccessMode) database.Server {
	t.Helper()

	db, err := sql.Open("postgres", testDSN())
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("skipping integration test: could not connect to local postgres at %s: %v", testDSN(), err)
	}
	db.Close()

	connection := config.Connection{
		Name:       "test",
		URI:        testDSN(),
		AccessMode: mode,
	}

	dbSrv := database.New(database.RegisterConnections(connection))
	t.Cleanup(dbSrv.Close)
	return dbSrv
}

// callTool sends a single tools/call request through srv.Run and returns
// the decoded response object.
func callTool(t *testing.T, srv *mcpserver.Server, name string, args map[string]any) map[string]any {
	t.Helper()

	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}
	reqJSON, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": json.RawMessage(argsJSON),
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	var out bytes.Buffer
	if err := srv.Run(context.Background(), strings.NewReader(string(reqJSON)+"\n"), &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

func TestRegisterTools_ListConnections(t *testing.T) {
	dbSrv := newTestServer(t, config.Unrestricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "list_connections", map[string]any{})

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	text := content["text"].(string)
	if !strings.Contains(text, "\"test\"") || !strings.Contains(text, "unrestricted") {
		t.Errorf("expected connection 'test' with mode 'unrestricted' in output: %s", text)
	}
}

func TestRegisterTools_ExecuteSQL_Success(t *testing.T) {
	dbSrv := newTestServer(t, config.Unrestricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "execute_sql", map[string]any{"connection": "test", "sql": "SELECT 1 AS one"})

	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("expected success, got error result: %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "\"one\"") {
		t.Errorf("expected column 'one' in output: %v", content["text"])
	}
}

func TestRegisterTools_ExecuteSQL_UnknownConnection(t *testing.T) {
	dbSrv := newTestServer(t, config.Unrestricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "execute_sql", map[string]any{"connection": "does-not-exist", "sql": "SELECT 1"})

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError true for unknown connection, got %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "unknown connection") {
		t.Errorf("expected 'unknown connection' message, got: %v", content["text"])
	}
}

func TestRegisterTools_ExecuteSQL_RestrictedBlocksWrite(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "execute_sql", map[string]any{"connection": "test", "sql": "DELETE FROM does_not_matter"})

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError true for restricted write, got %v", result)
	}
}

func TestRegisterTools_ExecuteSQL_InvalidArguments(t *testing.T) {
	dbSrv := newTestServer(t, config.Unrestricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "execute_sql",
			"arguments": json.RawMessage(`"not-an-object"`),
		},
	})

	var out bytes.Buffer
	if err := srv.Run(context.Background(), bytes.NewReader(append(reqJSON, []byte("\n")...)), &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected JSON-RPC error for malformed arguments, got %v", resp)
	}
}

func TestRegisterTools_ExplainQuery(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "explain_query", map[string]any{"connection": "test", "sql": "SELECT 1", "analyze": false})

	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("expected success, got error result: %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "Plan") {
		t.Errorf("expected plan JSON in output: %v", content["text"])
	}
}

func TestRegisterTools_ExplainQuery_UnknownConnection(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "explain_query", map[string]any{"connection": "nope", "sql": "SELECT 1"})

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError true for unknown connection, got %v", result)
	}
}

func TestRegisterTools_ExplainQuery_InvalidArguments(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "explain_query",
			"arguments": json.RawMessage(`"not-an-object"`),
		},
	})

	var out bytes.Buffer
	if err := srv.Run(context.Background(), strings.NewReader(string(reqJSON)+"\n"), &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected JSON-RPC error for malformed arguments, got %v", resp)
	}
}

func TestRegisterTools_ListSchemas(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "list_schemas", map[string]any{"connection": "test"})

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "public") {
		t.Errorf("expected 'public' schema in output: %v", content["text"])
	}
}

func TestRegisterTools_ListSchemas_UnknownConnection(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "list_schemas", map[string]any{"connection": "nope"})

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError true for unknown connection, got %v", result)
	}
}

func TestRegisterTools_ListObjects(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "list_objects", map[string]any{"connection": "test", "schema_name": "pg_catalog", "object_type": "extension"})

	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("expected success, got error result: %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "plpgsql") {
		t.Errorf("expected 'plpgsql' extension in output: %v", content["text"])
	}
}

func TestRegisterTools_ListObjects_UnknownConnection(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	resp := callTool(t, srv, "list_objects", map[string]any{"connection": "nope", "schema_name": "public"})

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError true for unknown connection, got %v", result)
	}
}

func TestRegisterTools_ListObjects_InvalidArguments(t *testing.T) {
	dbSrv := newTestServer(t, config.Restricted)
	srv := mcpserver.NewServer("postgres-mcp", serverVersion)
	registerTools(srv, dbSrv)

	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_objects",
			"arguments": json.RawMessage(`"not-an-object"`),
		},
	})

	var out bytes.Buffer
	if err := srv.Run(context.Background(), strings.NewReader(string(reqJSON)+"\n"), &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected JSON-RPC error for malformed arguments, got %v", resp)
	}
}

func TestMarshalConnectionList(t *testing.T) {
	connection := config.Connection{
		Name:       "a",
		URI:        "postgres://nope@nowhere.invalid:1/db",
		AccessMode: config.Restricted,
	}

	// marshalConnectionList only reads registered configuration (via
	// dbSrv.List()), so this never needs to actually connect.
	dbSrv := database.New(database.RegisterConnections(connection))

	text, err := marshalConnectionList(dbSrv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "\"a\"") || !strings.Contains(text, "restricted") {
		t.Errorf("unexpected output: %s", text)
	}
}
