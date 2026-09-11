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

// --- run() tests ---
//
// run(args) takes connections as repeated -connection flags (one JSON
// object per flag) instead of reading them from the environment; it still
// reads os.Stdin/os.Stdout directly (via internal.Serve). withStdio below
// swaps the real os.Stdin/os.Stdout for pipes so these tests can still
// feed it a request and capture its response without going through a
// real terminal. connectionArgs is a small helper that builds the
// []string of "-connection=<json>" args from a slice of connection
// objects.

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
		{"name": "one", "uri": "postgres://postgres@unreachable-host-1.invalid:5432/db"},
		{"name": "two", "uri": "postgres://postgres@unreachable-host-2.invalid:5432/db"},
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
