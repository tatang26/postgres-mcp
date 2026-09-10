package database

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// queryResult is the JSON shape returned as tool output text for any query
// that produces a result set.
type queryResult struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Count   int              `json:"row_count"`
	// Truncated is true if Rows was cut short of the query's actual result
	// set because it hit database.Limits.MaxRows or MaxBytes; see
	// TruncatedReason for which one.
	Truncated       bool   `json:"truncated,omitzero"`
	TruncatedReason string `json:"truncated_reason,omitzero"`
}

func (q queryResult) String() (string, error) {
	b, err := json.Marshal(q, jsontext.WithIndent("  "))
	if err != nil {
		return "", fmt.Errorf("failed to marshal query result: %w", err)
	}

	return string(b), nil
}

// statusResult is the JSON shape returned for a statement that executed
// successfully but produced no result set (e.g. DDL, or DML without
// RETURNING). database/sql does not expose the affected-row count from
// QueryContext (only from ExecContext), so this intentionally does not
// report one; callers that need affected-row counts should add a RETURNING
// clause.
type statusResult struct {
	Status string `json:"status"`
}

func (s statusResult) String() (string, error) {
	b, err := json.Marshal(s, jsontext.WithIndent("  "))
	if err != nil {
		return "", fmt.Errorf("failed to marshal status result: %w", err)
	}

	return string(b), nil
}
