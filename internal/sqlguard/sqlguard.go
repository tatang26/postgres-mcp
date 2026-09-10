// Package sqlguard provides a conservative, app-level classifier that decides
// whether a SQL statement is safe to run when a connection is opened in
// "restricted" (read-only) access mode.
//
// This is NOT a full SQL parser. It strips comments and string/identifier
// literals, then applies keyword allow/deny lists. Because it is text-based,
// it is intentionally conservative: anything it cannot confidently classify
// as read-only is rejected. In particular:
//
//   - EXPLAIN of a write statement is rejected even though a plain EXPLAIN
//     (without ANALYZE) never executes the statement. This keeps behavior
//     simple and predictable rather than trying to special-case ANALYZE.
//   - Data-modifying CTEs (e.g. "WITH x AS (DELETE FROM t RETURNING *) ...")
//     are rejected because the DELETE/UPDATE/INSERT/MERGE keyword is
//     present anywhere in the statement, not just as the leading keyword.
//     Every other write/DDL/session command is only ever valid as a
//     statement's leading keyword in Postgres, so it's caught by the
//     leading-keyword allowlist alone without needing an "anywhere" scan
//     (which would otherwise misclassify ordinary identifiers like a
//     column named comment, start, or set).
//   - Side-effecting function calls (e.g. pg_terminate_backend, setval) are
//     NOT detected at the text level.
//
// This package is defense-in-depth, not the primary guarantee: callers
// should additionally run restricted-mode queries inside a database-level
// read-only transaction (sql.TxOptions{ReadOnly: true}), which Postgres
// itself enforces and which this text-based guard cannot fully replace.
package sqlguard

import (
	"fmt"
	"regexp"
	"strings"
)

// allowedLeading is the set of statement-leading keywords considered safe to
// run in restricted mode.
var allowedLeading = map[string]bool{
	"SELECT":  true,
	"WITH":    true,
	"EXPLAIN": true,
	"SHOW":    true,
	"TABLE":   true,
	"VALUES":  true,
}

// deniedKeywords is the set of keywords that, if present anywhere at the
// text level in a statement, mark it as unsafe for restricted mode.
//
// This list is intentionally narrow. Every other write/DDL/session command
// (DROP, CREATE, ALTER, TRUNCATE, GRANT, REVOKE, COPY, VACUUM, SET, CALL,
// DO, BEGIN, COMMIT, COMMENT, START, etc.) is only ever valid as the
// leading statement of a top-level SQL statement in Postgres - never as
// the body of a WITH CTE or as an argument/identifier elsewhere - so the
// allowedLeading check above already rejects every statement starting
// with one of them, for every statement produced by splitting on ';'.
// Scanning for them "anywhere" in the text would only produce false
// positives (e.g. a column literally named comment, start, or set) without
// adding any real coverage.
//
// INSERT, UPDATE, DELETE, and MERGE are different: Postgres allows a
// data-modifying statement as a CTE body (e.g. "WITH x AS (DELETE FROM t
// RETURNING *) SELECT * FROM x") or as the trailing primary statement of a
// WITH clause, so they must still be caught even when they are not the
// statement's leading keyword. UPSERT isn't real Postgres syntax but is
// kept for defense in depth in case of dialect confusion.
var deniedKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE", "UPSERT",
}

var deniedKeywordRe = regexp.MustCompile(
	`(?i)\b(` + strings.Join(deniedKeywords, "|") + `)\b`,
)

// leadingWordRe extracts the first identifier-like token of a statement.
var leadingWordRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

// Error describes why a statement was rejected in restricted mode.
type Error struct {
	Statement string
	Keyword   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("restricted mode: statement not allowed (keyword %q): %s", e.Keyword, truncate(e.Statement, 120))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// CheckReadOnly returns a non-nil *Error if sql contains any statement that
// is not considered safe to execute in restricted (read-only) mode. sql may
// contain multiple ';'-separated statements; every statement must pass.
func CheckReadOnly(sql string) error {
	cleaned := stripCommentsAndLiterals(sql)

	for stmt := range strings.SplitSeq(cleaned, ";") {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}

		// A statement may legitimately start with one or more opening
		// parens (e.g. "(SELECT 1) UNION (SELECT 2)"); skip them before
		// extracting the leading keyword. This doesn't weaken the check:
		// if what follows the parens isn't an allowed keyword, it's still
		// rejected below exactly as if the parens weren't there.
		leadingSource := strings.TrimLeft(trimmed, "( \t\r\n")
		leading := strings.ToUpper(leadingWordRe.FindString(leadingSource))
		if leading == "" || !allowedLeading[leading] {
			return &Error{Statement: trimmed, Keyword: leading}
		}

		if m := deniedKeywordRe.FindString(trimmed); m != "" {
			return &Error{Statement: trimmed, Keyword: strings.ToUpper(m)}
		}
	}

	return nil
}

// stripCommentsAndLiterals returns a copy of sql with the contents of line
// comments, block comments, single-quoted string literals, double-quoted
// identifiers, and dollar-quoted string literals blanked out (replaced with
// spaces), while leaving everything else - including semicolon positions
// outside of literals - intact. This lets later keyword scanning ignore
// keywords that only appear inside literals/comments.
func stripCommentsAndLiterals(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))

	n := len(sql)
	i := 0
	for i < n {
		c := sql[i]

		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			j := i
			for j < n && sql[j] != '\n' {
				j++
			}
			i = j

		case c == '/' && i+1 < n && sql[i+1] == '*':
			j := i + 2
			for j+1 < n && !(sql[j] == '*' && sql[j+1] == '/') {
				j++
			}
			if j+1 < n {
				j += 2
			} else {
				j = n
			}
			// Comments are token separators in real SQL, so replace with a
			// single space rather than deleting outright (avoids gluing
			// adjacent tokens together, e.g. "SEL/*x*/ECT" must not become
			// "SELECT").
			b.WriteByte(' ')
			i = j

		case c == '\'':
			j := skipQuoted(sql, i, '\'')
			b.WriteByte(' ')
			i = j

		case c == '"':
			j := skipQuoted(sql, i, '"')
			b.WriteByte(' ')
			i = j

		case c == '$':
			if end, ok := skipDollarQuoted(sql, i); ok {
				b.WriteByte(' ')
				i = end
			} else {
				b.WriteByte(c)
				i++
			}

		default:
			b.WriteByte(c)
			i++
		}
	}

	return b.String()
}

// skipQuoted returns the index just past the closing quote of a
// quote-delimited token starting at sql[start] (sql[start] == quote), honoring
// the SQL convention that a doubled quote ("" or ”) is an escaped quote
// inside the token.
func skipQuoted(sql string, start int, quote byte) int {
	n := len(sql)
	j := start + 1
	for j < n {
		if sql[j] == quote {
			if j+1 < n && sql[j+1] == quote {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return n
}

// skipDollarQuoted attempts to parse a dollar-quoted string starting at
// sql[start] (sql[start] == '$'), e.g. $$...$$ or $tag$...$tag$. It returns
// the index just past the closing tag and true on success, or false if
// sql[start] does not begin a valid dollar-quote.
func skipDollarQuoted(sql string, start int) (int, bool) {
	n := len(sql)
	j := start + 1
	for j < n && (isAlnum(sql[j]) || sql[j] == '_') {
		j++
	}
	if j >= n || sql[j] != '$' {
		return 0, false
	}
	tag := sql[start : j+1]

	rest := sql[j+1:]
	idx := strings.Index(rest, tag)
	if idx < 0 {
		return n, true
	}
	return j + 1 + idx + len(tag), true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
