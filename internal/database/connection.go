package database

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"postgres-mcp/internal/database/config"
	"postgres-mcp/internal/sqlguard"
)

// validObjectTypes are the object_type values accepted by ListObjects.
var validObjectTypes = map[string]bool{
	"table":     true,
	"view":      true,
	"sequence":  true,
	"extension": true,
}

// auditLog writes one structured record per executed statement to
// stderr - never stdout, which is the MCP stdio JSON-RPC transport - so
// there's always a record of which connection ran what, in which access
// mode, for how long, and whether it failed. This is intentionally basic
// (no rotation, no external sink); operators who need more should tee
// stderr into their own log pipeline.
var auditLog = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// maxAuditSQLLen bounds how much of a statement's text is included in an
// audit log record.
const maxAuditSQLLen = 2000

func truncateForLog(s string) string {
	if len(s) <= maxAuditSQLLen {
		return s
	}
	return s[:maxAuditSQLLen] + "...(truncated)"
}

type Connection struct {
	DB     *sql.DB
	Mode   config.AccessMode
	Limits Limits
	// Name is this connection's registered name, used only for audit
	// logging.
	Name string
}

// logQuery emits one audit log record for a single statement execution
// (see auditLog).
func (c *Connection) logQuery(operation, query string, start time.Time, err error) {
	attrs := []any{
		slog.String("connection", c.Name),
		slog.String("access_mode", c.Mode.String()),
		slog.String("operation", operation),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("sql", truncateForLog(query)),
	}
	if err != nil {
		auditLog.Error("query", append(attrs, slog.String("error", err.Error()))...)
		return
	}
	auditLog.Info("query", attrs...)
}

func (c *Connection) checkReadOnly(query string) error {
	if c.Mode != config.Restricted {
		return nil
	}

	return sqlguard.CheckReadOnly(query)
}

// queryRows executes query against the connection and returns the
// resulting rows plus a cleanup func the caller must invoke (after rows
// have been closed, e.g. by rowsToJSON) to release any transaction opened
// for it.
//
// In restricted mode, the query runs inside a database-enforced read-only
// transaction (sql.TxOptions{ReadOnly: true}). This is the primary
// enforcement of restricted mode: sqlguard's text-level check (see
// checkReadOnly) is a fast-fail heuristic layered in front of it, but
// Postgres itself is what actually guarantees no write takes effect, even
// against a write attempt sqlguard's text-level scan doesn't recognize
// (e.g. a side-effecting function call).
func (c *Connection) queryRows(ctx context.Context, query string) (*sql.Rows, func(), error) {
	if c.Mode != config.Restricted {
		rows, err := c.DB.QueryContext(ctx, query)
		if err != nil {
			return nil, nil, err
		}
		return rows, func() {}, nil
	}

	tx, err := c.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to begin read-only transaction: %w", err)
	}

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	// Nothing executed in restricted mode should ever be committed; the
	// transaction is always rolled back once the caller is done with rows.
	return rows, func() { tx.Rollback() }, nil
}

func (c *Connection) ExecuteQuery(ctx context.Context, query string) (string, error) {
	start := time.Now()
	result, err := c.executeQuery(ctx, query)
	c.logQuery("execute_sql", query, start, err)
	return result, err
}

func (c *Connection) executeQuery(ctx context.Context, query string) (string, error) {
	if err := c.checkReadOnly(query); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.Limits.QueryTimeout)
	defer cancel()

	rows, cleanup, err := c.queryRows(ctx, query)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	defer cleanup()

	return c.rowsToJSON(rows)
}

// ExplainQuery returns the Postgres query plan for query, in JSON format.
// If analyze is true, Postgres actually executes the query to gather real
// timing/row-count statistics (EXPLAIN ANALYZE); in restricted mode the
// same sqlguard check as ExecuteQuery is applied regardless of analyze,
// since a plain (non-ANALYZE) EXPLAIN of a write statement is harmless but
// this server intentionally keeps the restricted-mode rule simple and
// uniform.
func (c *Connection) ExplainQuery(ctx context.Context, query string, analyze bool) (string, error) {
	start := time.Now()
	result, err := c.explainQuery(ctx, query, analyze)
	op := "explain_query"
	if analyze {
		op = "explain_query(analyze)"
	}
	c.logQuery(op, query, start, err)
	return result, err
}

func (c *Connection) explainQuery(ctx context.Context, query string, analyze bool) (string, error) {
	if err := c.checkReadOnly(query); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.Limits.QueryTimeout)
	defer cancel()

	var builder strings.Builder
	builder.WriteString("EXPLAIN (FORMAT JSON")
	if analyze {
		builder.WriteString(", ANALYZE")
	}
	builder.WriteString(") ")
	builder.WriteString(query)

	// EXPLAIN ANALYZE actually executes query, so this goes through
	// queryRows the same as ExecuteQuery, to get the same restricted-mode
	// read-only transaction enforcement.
	rows, cleanup, err := c.queryRows(ctx, builder.String())
	if err != nil {
		return "", fmt.Errorf("explain failed: %w", err)
	}
	defer cleanup()
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("explain failed: %w", err)
		}
		return "", fmt.Errorf("explain produced no output")
	}

	var plan string
	if err := rows.Scan(&plan); err != nil {
		return "", fmt.Errorf("failed to read explain output: %w", err)
	}

	return plan, nil
}

// ListSchemas returns the list of schemas in the connected database, in
// JSON format. This is always permitted, regardless of access mode.
func (c *Connection) ListSchemas(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Limits.QueryTimeout)
	defer cancel()

	stmt := `
		SELECT schema_name
		FROM information_schema.schemata
		ORDER BY schema_name
	`

	rows, err := c.DB.QueryContext(ctx, stmt)
	if err != nil {
		return "", fmt.Errorf("failed to list schemas: %w", err)
	}

	return c.rowsToJSON(rows)
}

// rowsToJSON drains rows into a queryResult, in JSON format. To bound this
// server's memory usage and the size of what gets dumped into an LLM's
// context, it stops early - marking the result Truncated - once either
// c.Limits.MaxRows rows or approximately c.Limits.MaxBytes bytes have been
// accumulated, whichever comes first. Rows beyond that point are never
// scanned or returned, but the query itself always runs to completion
// server-side (Postgres doesn't support only fetching a byte prefix of a
// result set); a real per-query row cap should additionally use LIMIT.
func (c *Connection) rowsToJSON(rows *sql.Rows) (string, error) {
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("failed to read columns: %w", err)
	}

	if len(cols) == 0 {
		return statusResult{Status: "ok"}.String()
	}

	result := queryResult{
		Columns: cols,
		Rows:    []map[string]any{},
	}

	maxRows := c.Limits.MaxRows
	maxBytes := c.Limits.MaxBytes
	approxBytes := 0

	for rows.Next() {
		if maxRows > 0 && len(result.Rows) >= maxRows {
			result.Truncated = true
			result.TruncatedReason = fmt.Sprintf("row_count limit of %d reached", maxRows)
			break
		}

		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", fmt.Errorf("failed to scan row: %w", err)
		}

		row := make(map[string]any, len(cols))
		for i, c := range cols {
			switch t := vals[i].(type) {
			case []byte:
				row[c] = string(t)
			case time.Time:
				row[c] = t.Format(time.RFC3339Nano)
			default:
				row[c] = t
			}
			approxBytes += len(c) + approxValueSize(row[c])
		}
		result.Rows = append(result.Rows, row)

		if maxBytes > 0 && approxBytes >= maxBytes {
			result.Truncated = true
			result.TruncatedReason = fmt.Sprintf("result size limit of %d bytes reached", maxBytes)
			break
		}
	}

	// If the loop above stopped early due to a limit rather than
	// exhausting rows.Next(), rows.Err() intentionally isn't checked: a
	// later error on a row we chose not to read is irrelevant to a result
	// we're already reporting as truncated.
	if !result.Truncated {
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("error reading rows: %w", err)
		}
	}

	result.Count = len(result.Rows)
	return result.String()
}

// approxValueSize returns a rough, cheap-to-compute estimate of v's
// contribution to the final JSON size, used only to enforce Limits.MaxBytes
// as a safety cap - not an exact byte count.
func approxValueSize(v any) int {
	switch t := v.(type) {
	case nil:
		return 4 // "null"
	case string:
		return len(t)
	case bool:
		return 5 // "false"
	default:
		return len(fmt.Sprint(t))
	}
}

// ListObjects returns the objects of objectType (table, view, sequence, or
// extension) within schemaName, in JSON format. This is always permitted,
// regardless of access mode.
func (c *Connection) ListObjects(ctx context.Context, schemaName, objectType string) (string, error) {
	objectType = cmp.Or(objectType, "table")

	if !validObjectTypes[objectType] {
		return "", fmt.Errorf("invalid object_type %q: must be one of table, view, sequence, extension", objectType)
	}

	if schemaName == "" {
		return "", fmt.Errorf("schema_name is required")
	}

	query, err := listObjectsQuery(objectType)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, c.Limits.QueryTimeout)
	defer cancel()

	rows, err := c.DB.QueryContext(ctx, query, schemaName)
	if err != nil {
		return "", fmt.Errorf("failed to list %s objects: %w", objectType, err)
	}

	return c.rowsToJSON(rows)
}

func listObjectsQuery(objectType string) (string, error) {
	switch objectType {
	case "table":
		return `
			SELECT table_name
			FROM information_schema.tables
			WHERE table_schema = $1 AND table_type = 'BASE TABLE'
			ORDER BY table_name
		`, nil
	case "view":
		return `
			SELECT table_name AS view_name
			FROM information_schema.views
			WHERE table_schema = $1
			ORDER BY table_name
		`, nil
	case "sequence":
		return `
			SELECT sequence_name
			FROM information_schema.sequences
			WHERE sequence_schema = $1
			ORDER BY sequence_name
		`, nil
	case "extension":
		return `
			SELECT e.extname AS extension_name
			FROM pg_extension e
			JOIN pg_namespace n ON n.oid = e.extnamespace
			WHERE n.nspname = $1
			ORDER BY e.extname
		`, nil
	default:
		return "", fmt.Errorf("invalid object_type %q", objectType)
	}
}
