package database

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"postgres-mcp/internal/database/config"
)

const defaultTestDSN = "postgres://postgres@localhost:5432/postgres?sslmode=disable"

func testDSN() string {
	if dsn := os.Getenv("PGTOOLS_TEST_DATABASE_URI"); dsn != "" {
		return dsn
	}
	return defaultTestDSN
}

// requireLocalPostgres skips the test if no Postgres is reachable at
// testDSN(), mirroring cmd/postgres-mcp's integration test setup.
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

func newTestManager(t *testing.T, mode config.AccessMode, opts ...option) Server {
	t.Helper()
	requireLocalPostgres(t)
	conn := config.Connection{Name: "test", URI: testDSN(), AccessMode: mode}
	allOpts := append([]option{RegisterConnections(conn)}, opts...)
	srv := New(allOpts...)
	t.Cleanup(srv.Close)
	return srv
}

func TestGet_UnknownConnection(t *testing.T) {
	requireLocalPostgres(t)
	conn := config.Connection{Name: "test", URI: testDSN(), AccessMode: config.Unrestricted}
	srv := New(RegisterConnections(conn))
	defer srv.Close()

	_, err := srv.Get(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown connection")
	}
	if !strings.Contains(err.Error(), "unknown connection") {
		t.Errorf("expected 'unknown connection' in error, got: %v", err)
	}
}

func TestGet_CachesConnection(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)

	c1, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c2, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c1 != c2 {
		t.Error("expected Get to return the same cached *Connection on repeated calls")
	}
}

func TestGet_ConcurrentCallsShareOneConnection(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)

	const n = 20
	results := make(chan *Connection, n)
	for i := 0; i < n; i++ {
		go func() {
			c, err := srv.Get(context.Background(), "test")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				results <- nil
				return
			}
			results <- c
		}()
	}

	first := <-results
	for i := 1; i < n; i++ {
		c := <-results
		if c != first {
			t.Error("expected all concurrent Get calls to return the same *Connection")
		}
	}
}

func TestGet_FailedPingIsNotCached(t *testing.T) {
	requireLocalPostgres(t)
	conn := config.Connection{Name: "bad", URI: "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1", AccessMode: config.Unrestricted}
	srv := New(RegisterConnections(conn))
	defer srv.Close()

	if _, err := srv.Get(context.Background(), "bad"); err == nil {
		t.Fatal("expected error connecting to an unreachable host")
	}
	// A second attempt must retry rather than replaying a cached failure
	// forever; it should fail again (still unreachable) rather than
	// panicking on a cached nil/broken *Connection.
	if _, err := srv.Get(context.Background(), "bad"); err == nil {
		t.Fatal("expected second attempt to also fail against an unreachable host")
	}
}

func TestList_SortedByName(t *testing.T) {
	requireLocalPostgres(t)
	zetaConn := config.Connection{Name: "zeta", URI: testDSN(), AccessMode: config.Restricted}
	alphaConn := config.Connection{Name: "alpha", URI: testDSN(), AccessMode: config.Unrestricted}
	srv := New(
		RegisterConnections(zetaConn, alphaConn),
	)
	defer srv.Close()

	infos := srv.List()
	if len(infos) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(infos))
	}
	if infos[0].Name != "alpha" || infos[1].Name != "zeta" {
		t.Errorf("expected sorted [alpha, zeta], got [%s, %s]", infos[0].Name, infos[1].Name)
	}
	if infos[0].AccessMode != config.Unrestricted {
		t.Errorf("expected alpha to be unrestricted, got %v", infos[0].AccessMode)
	}
}

func TestClose_ClosesUnderlyingPoolAndAllowsReconnect(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)

	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	srv.Close()

	if err := conn.DB.PingContext(context.Background()); err == nil {
		t.Error("expected the underlying *sql.DB to be closed after Server.Close")
	}

	// Get after Close must not hand back the closed pool.
	conn2, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error reconnecting after Close: %v", err)
	}
	if err := conn2.DB.PingContext(context.Background()); err != nil {
		t.Errorf("expected reconnected pool to be usable, got: %v", err)
	}
}

func TestConnect_AppliesPoolLimits(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)

	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stats := conn.DB.Stats()
	if stats.MaxOpenConnections != maxOpenConns {
		t.Errorf("expected MaxOpenConnections=%d, got %d", maxOpenConns, stats.MaxOpenConnections)
	}
}

func TestConnect_AppliesLimitsFromWithLimits(t *testing.T) {
	requireLocalPostgres(t)
	custom := Limits{QueryTimeout: 42 * time.Second, MaxRows: 7, MaxBytes: 123}
	regConn := config.Connection{Name: "test", URI: testDSN(), AccessMode: config.Unrestricted}
	srv := New(RegisterConnections(regConn), WithLimits(custom))
	defer srv.Close()

	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.Limits != custom {
		t.Errorf("expected connection to inherit manager Limits %+v, got %+v", custom, conn.Limits)
	}
}

// --- Connection method tests ---

func execToJSON(t *testing.T, c *Connection, sql string) (string, error) {
	t.Helper()
	return c.ExecuteQuery(context.Background(), sql)
}

func TestExecuteQuery_Select(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := execToJSON(t, conn, "SELECT 1 AS one, 'x' AS two")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"one"`) || !strings.Contains(out, `"two"`) {
		t.Errorf("expected columns 'one' and 'two' in output: %s", out)
	}
}

func TestExecuteQuery_NoResultSet(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A regular (non-temp) table, since separate ExecuteQuery calls may be
	// served by different pooled physical connections, and a TEMP table is
	// only visible on the session that created it.
	if _, err := execToJSON(t, conn, "CREATE TABLE no_result_test_table (id int)"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		execToJSON(t, conn, "DROP TABLE IF EXISTS no_result_test_table")
	})

	out, err := execToJSON(t, conn, "INSERT INTO no_result_test_table (id) VALUES (1)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"status"`) {
		t.Errorf("expected status result for a no-result-set statement, got: %s", out)
	}
}

func TestExecuteQuery_RestrictedRejectsWrite(t *testing.T) {
	srv := newTestManager(t, config.Restricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := execToJSON(t, conn, "DELETE FROM pg_class"); err == nil {
		t.Fatal("expected restricted mode to reject a DELETE")
	}
}

func TestExecuteQuery_RestrictedEnforcedAtDatabaseLevel(t *testing.T) {
	// This verifies the read-only transaction enforcement, not just the
	// sqlguard text check: a write statement disguised well enough to
	// pass sqlguard would still be rejected by Postgres itself. We can't
	// easily disguise a write from sqlguard here, but we can verify that
	// a restricted-mode connection's queries run inside a read-only
	// transaction (Postgres rejects any actual write attempted within
	// one, independent of sqlguard).
	srv := newTestManager(t, config.Restricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows, cleanup, err := conn.queryRows(context.Background(), "SELECT set_config('foo', 'bar', false)")
	if err == nil {
		rows.Close()
		cleanup()
		t.Fatal("expected a write attempt (set_config) inside a read-only transaction to fail at the database level")
	}
}

func TestExecuteQuery_RowLimitTruncates(t *testing.T) {
	requireLocalPostgres(t)
	regConn := config.Connection{Name: "test", URI: testDSN(), AccessMode: config.Unrestricted}
	srv := New(RegisterConnections(regConn), WithLimits(Limits{
		QueryTimeout: 5 * time.Second,
		MaxRows:      3,
		MaxBytes:     1 << 20,
	}))
	defer srv.Close()

	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := execToJSON(t, conn, "SELECT generate_series(1, 10) AS n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed struct {
		RowCount        int    `json:"row_count"`
		Truncated       bool   `json:"truncated"`
		TruncatedReason string `json:"truncated_reason"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if parsed.RowCount != 3 {
		t.Errorf("expected row_count=3, got %d", parsed.RowCount)
	}
	if !parsed.Truncated {
		t.Error("expected truncated=true")
	}
	if !strings.Contains(parsed.TruncatedReason, "row_count") {
		t.Errorf("expected truncated_reason to mention row_count, got: %s", parsed.TruncatedReason)
	}
}

func TestExecuteQuery_QueryTimeoutCancelsSlowQuery(t *testing.T) {
	requireLocalPostgres(t)
	regConn := config.Connection{Name: "test", URI: testDSN(), AccessMode: config.Unrestricted}
	srv := New(RegisterConnections(regConn), WithLimits(Limits{
		QueryTimeout: 200 * time.Millisecond,
		MaxRows:      1000,
		MaxBytes:     1 << 20,
	}))
	defer srv.Close()

	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	start := time.Now()
	_, err = execToJSON(t, conn, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected pg_sleep(5) to be canceled by the query timeout")
	}
	if elapsed > 4*time.Second {
		t.Errorf("expected the query to be canceled well before pg_sleep(5) completed, took %v", elapsed)
	}
}

func TestExplainQuery_ReturnsPlan(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := conn.ExplainQuery(context.Background(), "SELECT 1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Plan") {
		t.Errorf("expected a query plan in output, got: %s", out)
	}
}

func TestExplainQuery_RestrictedRejectsWrite(t *testing.T) {
	srv := newTestManager(t, config.Restricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.ExplainQuery(context.Background(), "DELETE FROM pg_class", false); err == nil {
		t.Fatal("expected restricted mode to reject explaining a DELETE")
	}
}

func TestListSchemas(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := conn.ListSchemas(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "public") {
		t.Errorf("expected 'public' schema in output: %s", out)
	}
}

func TestListSchemas_PermittedInRestrictedMode(t *testing.T) {
	srv := newTestManager(t, config.Restricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.ListSchemas(context.Background()); err != nil {
		t.Errorf("expected ListSchemas to be permitted in restricted mode, got: %v", err)
	}
}

func TestListObjects_Table(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A pool may serve each query from a different physical backend
	// connection, so pg_temp objects (session-scoped) aren't reliably
	// visible across separate calls here; use a real table in "public"
	// instead, dropped afterward.
	if _, err := execToJSON(t, conn, "CREATE TABLE list_objects_test_table (id int)"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		execToJSON(t, conn, "DROP TABLE IF EXISTS list_objects_test_table")
	})

	out, err := conn.ListObjects(context.Background(), "public", "table")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "list_objects_test_table") {
		t.Errorf("expected 'list_objects_test_table' in output: %s", out)
	}
}

func TestListObjects_DefaultsToTable(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.ListObjects(context.Background(), "public", ""); err != nil {
		t.Errorf("unexpected error with empty object_type: %v", err)
	}
}

func TestListObjects_InvalidObjectType(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.ListObjects(context.Background(), "public", "bogus"); err == nil {
		t.Fatal("expected error for invalid object_type")
	}
}

func TestListObjects_MissingSchemaName(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.ListObjects(context.Background(), "", "table"); err == nil {
		t.Fatal("expected error for missing schema_name")
	}
}

func TestListObjects_View(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := execToJSON(t, conn, "CREATE VIEW list_objects_test_view AS SELECT 1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		execToJSON(t, conn, "DROP VIEW IF EXISTS list_objects_test_view")
	})

	out, err := conn.ListObjects(context.Background(), "public", "view")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "list_objects_test_view") {
		t.Errorf("expected 'list_objects_test_view' in output: %s", out)
	}
}

func TestListObjects_Sequence(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := execToJSON(t, conn, "CREATE SEQUENCE list_objects_test_seq"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() {
		execToJSON(t, conn, "DROP SEQUENCE IF EXISTS list_objects_test_seq")
	})

	out, err := conn.ListObjects(context.Background(), "public", "sequence")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "list_objects_test_seq") {
		t.Errorf("expected 'list_objects_test_seq' in output: %s", out)
	}
}

func TestListObjects_Extension(t *testing.T) {
	srv := newTestManager(t, config.Unrestricted)
	conn, err := srv.Get(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// plpgsql is installed by default in every Postgres database.
	out, err := conn.ListObjects(context.Background(), "pg_catalog", "extension")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "plpgsql") {
		t.Errorf("expected 'plpgsql' extension in output: %s", out)
	}
}

func TestApproxValueSize(t *testing.T) {
	cases := []struct {
		v    any
		want int
	}{
		{nil, 4},
		{"abc", 3},
		{true, 5},
		{42, 2},
	}
	for _, c := range cases {
		if got := approxValueSize(c.v); got != c.want {
			t.Errorf("approxValueSize(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	if l.QueryTimeout <= 0 || l.MaxRows <= 0 || l.MaxBytes <= 0 {
		t.Errorf("expected all positive defaults, got %+v", l)
	}
}
