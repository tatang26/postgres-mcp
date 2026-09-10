package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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

func requireLocalPostgres(t *testing.T) {
	t.Helper()
	db, err := sql.Open("postgres", testDSN())
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipping integration test: could not connect to local postgres: %v", err)
	}
}

// setupTestDB connects to a local Postgres instance, skipping the test if
// none is reachable. registerTools' handlers are exercised here purely as
// argument-parsing/wiring glue (already-tested SQL behavior lives in
// internal/database), so no fixture data is needed.
func setupTestDB(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { db.Close() })

	return db
}

// newTestServer builds a database.Server with a single connection named
// "test", pointed at a real local Postgres instance. The connection is
// registered (not yet opened - Server connects lazily on first Get);
// requireLocalPostgres skips the test upfront if nothing is listening.
func newTestServer(t *testing.T, mode config.AccessMode) database.Server {
	t.Helper()
	requireLocalPostgres(t)
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

// --- run() tests ---
//
// run(args) takes connections as repeated -connection flags (one JSON
// object per flag) instead of reading them from the environment; it still
// reads os.Stdin/os.Stdout directly. withStdio below swaps the real
// os.Stdin/os.Stdout for pipes so these tests can still feed it a request
// and capture its response without going through a real terminal.
// connectionArgs is a small helper that builds the []string of
// "-connection=<json>" args from a slice of connection objects.

// connectionArgs marshals each entry in conns to JSON and returns it as a
// "-connection=<json>" flag, one per entry, suitable for passing to run.
func connectionArgs(t *testing.T, conns []map[string]any) []string {
	t.Helper()
	args := make([]string, len(conns))
	for i, c := range conns {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("failed to marshal connection %d: %v", i, err)
		}
		args[i] = "-connection=" + string(b)
	}
	return args
}

// withStdio temporarily replaces os.Stdin and os.Stdout for the duration
// of fn, feeding it input on stdin and capturing everything it writes to
// stdout, then restores the real file descriptors before returning. Must
// not be called from a goroutine other than the one holding exclusive
// access to the swapped globals for its duration (safe here because each
// test either calls it directly or owns the single worker goroutine that
// does).
func withStdio(input string, fn func() error) (stdout string, err error) {
	origStdin, origStdout := os.Stdin, os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	inR, inW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return "", fmt.Errorf("failed to create stdin pipe: %w", pipeErr)
	}
	outR, outW, pipeErr := os.Pipe()
	if pipeErr != nil {
		inR.Close()
		inW.Close()
		return "", fmt.Errorf("failed to create stdout pipe: %w", pipeErr)
	}

	os.Stdin = inR
	os.Stdout = outW

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, outR)
		outCh <- buf.String()
	}()

	if _, writeErr := inW.WriteString(input); writeErr != nil {
		inW.Close()
		return "", fmt.Errorf("failed to write stdin: %w", writeErr)
	}
	inW.Close()

	err = fn()

	outW.Close()
	stdout = <-outCh

	inR.Close()
	outR.Close()

	return stdout, err
}

func TestRun_MissingConnections(t *testing.T) {
	_, err := withStdio("", func() error { return run(nil) })
	if err == nil {
		t.Fatal("expected error when no -connection flags are given")
	}
	if !strings.Contains(err.Error(), "-connection") {
		t.Errorf("expected error to mention -connection, got: %v", err)
	}
}

func TestRun_InvalidConnectionsJSON(t *testing.T) {
	args := []string{"-connection=not json"}

	_, err := withStdio("", func() error { return run(args) })
	if err == nil {
		t.Fatal("expected error for invalid -connection JSON")
	}
}

// TestRun_NeverBlocksOnUnreachableConnections is the regression test for
// the real production incident that motivated lazy connections: run() used
// to open+ping every configured connection sequentially before ever
// responding to the MCP handshake, which took ~34s across 11 real
// connections (3 of them remote) and tripped opencode's startup timeout,
// marking the server as failed. Registering a connection must never
// perform I/O, and run() must be able to serve requests (including for
// completely unreachable connections, which only fail when actually used)
// near-instantly regardless of how many connections are configured.
func TestRun_NeverBlocksOnUnreachableConnections(t *testing.T) {
	args := connectionArgs(t, []map[string]any{
		{"uri": "postgres://postgres@unreachable-host-1.invalid:5432/db"},
		{"uri": "postgres://postgres@unreachable-host-2.invalid:5432/db"},
	})

	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_connections",
			"arguments": json.RawMessage(`{}`),
		},
	})

	type runResult struct {
		stdout string
		err    error
	}
	done := make(chan runResult, 1)
	start := time.Now()
	go func() {
		stdout, err := withStdio(string(reqJSON)+"\n", func() error { return run(args) })
		done <- runResult{stdout: stdout, err: err}
	}()

	var res runResult
	select {
	case res = <-done:
		if res.err != nil {
			t.Fatalf("expected run to succeed even with unreachable connections: %v", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run() blocked for >3s on unreachable connections; connections must be registered lazily, not connected at startup")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("run() took %v; expected near-instant startup regardless of connection reachability", elapsed)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (raw: %s)", err, res.stdout)
	}
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	text := content["text"].(string)
	// Both connections must still be listed: List() reflects configuration,
	// not live connectivity, since neither has ever been used yet.
	if strings.Count(text, "\"access_mode\"") != 2 {
		t.Errorf("expected both unreachable-but-registered connections to be listed: %s", text)
	}
}

func TestRun_ToolCallAgainstUnreachableConnection_FailsGracefully(t *testing.T) {
	args := connectionArgs(t, []map[string]any{
		{"name": "bad", "uri": "postgres://postgres@localhost:1/postgres?sslmode=disable&connect_timeout=1"},
	})

	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "execute_sql",
			"arguments": json.RawMessage(`{"connection":"bad","sql":"SELECT 1"}`),
		},
	})

	stdout, err := withStdio(string(reqJSON)+"\n", func() error { return run(args) })
	if err != nil {
		t.Fatalf("expected run() itself to succeed (process-level), got: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (raw: %s)", err, stdout)
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected the tool call itself to report isError for an unreachable connection, got: %v", result)
	}
}

func TestRun_Success_EndToEnd(t *testing.T) {
	requireLocalPostgres(t)

	args := connectionArgs(t, []map[string]any{
		{"name": "test", "uri": testDSN()},
	})

	reqJSON, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "execute_sql",
			"arguments": json.RawMessage(`{"connection":"test","sql":"SELECT 1 AS one"}`),
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	stdout, err := withStdio(string(reqJSON)+"\n", func() error { return run(args) })
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &resp); err != nil {
		t.Fatalf("failed to decode response: %v (raw: %s)", err, stdout)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %v", resp)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "\"one\"") {
		t.Errorf("expected column 'one' in output: %v", content["text"])
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
