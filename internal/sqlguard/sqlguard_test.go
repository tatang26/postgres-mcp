package sqlguard

import (
	"strings"
	"testing"
)

func TestCheckReadOnly_Allowed(t *testing.T) {
	cases := []string{
		"SELECT * FROM users",
		"select * from users where id = 1",
		"  SELECT 1  ",
		"SELECT * FROM users;",
		"SELECT * FROM users; SELECT * FROM orders",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"EXPLAIN SELECT * FROM users",
		"EXPLAIN (FORMAT JSON) SELECT * FROM users",
		"SHOW search_path",
		"TABLE users",
		"VALUES (1, 2), (3, 4)",
		"SELECT * FROM users WHERE note = 'please DELETE this row'",
		"SELECT * FROM \"delete\"",
		"SELECT delete_flag, updated_at FROM users",
		"SELECT commit_hash FROM releases",
		"-- comment mentioning DROP TABLE\nSELECT 1",
		"/* block comment with INSERT INTO x */ SELECT 1",
		"SELECT $$literal with DELETE inside$$",
		"SELECT $tag$literal with DROP TABLE inside$tag$",
		"",
		";;;",
		// Regression: these are ordinary identifiers that happen to
		// collide with statement-leading-only keywords. None of these
		// keywords can ever appear as a CTE body, so they must not be
		// flagged when they're just column/table names.
		"SELECT comment FROM posts",
		"SELECT start, stop FROM events",
		"SELECT id FROM t ORDER BY set",
		"SELECT id FROM t WHERE lock = true",
		"SELECT begin, commit FROM ledger",
		// Regression: a leading paren-wrapped SELECT must still be
		// recognized as read-only.
		"(SELECT 1) UNION (SELECT 2)",
		"  (SELECT 1) UNION (SELECT 2)",
	}

	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			if err := CheckReadOnly(sql); err != nil {
				t.Errorf("expected %q to be allowed, got error: %v", sql, err)
			}
		})
	}
}

func TestCheckReadOnly_Denied(t *testing.T) {
	cases := map[string]string{
		"INSERT INTO users (name) VALUES ('a')":                     "INSERT",
		"insert into users (name) values ('a')":                     "INSERT",
		"UPDATE users SET name = 'a'":                               "UPDATE",
		"DELETE FROM users":                                         "DELETE",
		"DROP TABLE users":                                          "DROP",
		"CREATE TABLE t (id int)":                                   "CREATE",
		"ALTER TABLE users ADD COLUMN x int":                        "ALTER",
		"TRUNCATE users":                                            "TRUNCATE",
		"GRANT SELECT ON users TO alice":                            "GRANT",
		"REVOKE SELECT ON users FROM alice":                         "REVOKE",
		"COPY users TO '/tmp/out.csv'":                              "COPY",
		"VACUUM users":                                              "VACUUM",
		"SET statement_timeout = 1000":                              "SET",
		"CALL my_procedure()":                                       "CALL",
		"DO $$ BEGIN NULL; END $$":                                  "DO",
		"WITH x AS (DELETE FROM users RETURNING *) SELECT * FROM x": "DELETE",
		"SELECT 1; DROP TABLE users":                                "DROP",
		"BEGIN":                                                     "BEGIN",
		"COMMIT":                                                    "COMMIT",
	}

	for sql, wantKeyword := range cases {
		t.Run(sql, func(t *testing.T) {
			err := CheckReadOnly(sql)
			if err == nil {
				t.Fatalf("expected %q to be denied, got no error", sql)
			}
			guardErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("expected *Error, got %T", err)
			}
			if !strings.EqualFold(guardErr.Keyword, wantKeyword) {
				t.Errorf("expected keyword %q, got %q (err: %v)", wantKeyword, guardErr.Keyword, err)
			}
		})
	}
}

func TestCheckReadOnly_UnknownLeadingKeywordDenied(t *testing.T) {
	if err := CheckReadOnly("FROBNICATE users"); err == nil {
		t.Fatal("expected unknown leading keyword to be denied")
	}
}

func TestError_MessageTruncatesLongStatements(t *testing.T) {
	long := strings.Repeat("x", 500)
	err := &Error{Statement: long, Keyword: "INSERT"}
	msg := err.Error()
	if len(msg) > 200 {
		t.Errorf("expected truncated error message, got length %d", len(msg))
	}
	if !strings.Contains(msg, "...") {
		t.Errorf("expected truncation marker in message: %s", msg)
	}
}

func TestStripCommentsAndLiterals(t *testing.T) {
	cases := map[string]string{
		"SELECT 1 -- trailing comment":   "SELECT 1 ",
		"SELECT 1 /* c */ FROM t":        "SELECT 1   FROM t",
		"SELECT 'it''s a test'":          "SELECT  ",
		"SELECT \"weird\"\"col\" FROM t": "SELECT   FROM t",
		"SELECT $$a;b$$":                 "SELECT  ",
		"SELECT $tag$a;b$tag$":           "SELECT  ",
		"SELECT 1; SELECT 2":             "SELECT 1; SELECT 2",
	}

	for in, want := range cases {
		got := stripCommentsAndLiterals(in)
		if got != want {
			t.Errorf("stripCommentsAndLiterals(%q) = %q, want %q", in, got, want)
		}
	}
}
