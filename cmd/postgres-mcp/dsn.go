package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// withDefaultSSLMode returns dsn with sslmode explicitly set if the caller
// didn't already specify one: "prefer" for a loopback host (local
// development), "require" otherwise.
//
// This matters because lib/pq's default (when sslmode is left unspecified)
// is "require" unconditionally, unlike psql/libpq's own default of
// "prefer". Defaulting to "require" everywhere would reject any connection
// to a server without SSL enabled at all (common for local development
// databases); defaulting to "prefer" everywhere would silently downgrade
// to plaintext against a misconfigured or attacker-controlled remote host
// that doesn't offer SSL, which is the wrong failure mode for anything
// that isn't local. So: "prefer" only for loopback, "require" for
// everything else.
func withDefaultSSLMode(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return withDefaultSSLModeURI(dsn)
	}
	return withDefaultSSLModeKeywordValue(dsn), nil
}

// isLoopbackHost reports whether host (as found in a connection URI or
// libpq "host" keyword) refers to the local machine. An empty host means
// libpq will use its own default (a Unix domain socket, or localhost),
// which is loopback.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func defaultSSLModeFor(host string) string {
	if isLoopbackHost(host) {
		return "prefer"
	}
	return "require"
}

func withDefaultSSLModeURI(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("invalid connection uri: not a valid URL")
	}

	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", defaultSSLModeFor(u.Hostname()))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// keywordValueHostRe extracts the value of a "host" keyword from a libpq
// keyword/value connection string, e.g. "host=localhost" or
// "host='my host'".
var keywordValueHostRe = regexp.MustCompile(`(?i)\bhost\s*=\s*(?:'([^']*)'|(\S+))`)

func withDefaultSSLModeKeywordValue(dsn string) string {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" || strings.Contains(trimmed, "sslmode=") {
		return dsn
	}

	host := ""
	if m := keywordValueHostRe.FindStringSubmatch(trimmed); m != nil {
		host = firstNonEmpty(m[1], m[2])
	}

	return trimmed + " sslmode=" + defaultSSLModeFor(host)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// withStatementTimeout returns dsn with a libpq "options" parameter that
// sets statement_timeout to timeout, so Postgres enforces the same query
// deadline server-side that this server enforces client-side via
// context.WithTimeout (see database.Limits.QueryTimeout). This covers the
// case where a client-side cancellation is lost (e.g. a network
// partition) but the query keeps running on the server. Skipped if the
// caller already configured connection options themselves.
func withStatementTimeout(dsn string, timeout time.Duration) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return withStatementTimeoutURI(dsn, timeout)
	}
	return withStatementTimeoutKeywordValue(dsn, timeout), nil
}

func withStatementTimeoutURI(dsn string, timeout time.Duration) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("invalid connection uri: not a valid URL")
	}

	q := u.Query()
	if q.Get("options") == "" {
		q.Set("options", fmt.Sprintf("-c statement_timeout=%d", timeout.Milliseconds()))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func withStatementTimeoutKeywordValue(dsn string, timeout time.Duration) string {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" || strings.Contains(trimmed, "options=") {
		return dsn
	}
	return fmt.Sprintf("%s options='-c statement_timeout=%d'", trimmed, timeout.Milliseconds())
}
