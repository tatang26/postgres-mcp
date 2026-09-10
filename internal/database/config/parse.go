// Parse parses the values of repeated -connection flags: each value is a
// single JSON object describing one Postgres connection this server should
// expose, e.g.:
//
//	-connection={"uri": "postgres://postgres@localhost:5432/byod_development", "access_mode": "unrestricted"}
//	-connection={"uri": "postgres://user:pass@prodcf.example.com:5432/byod"}
//
// "access_mode" defaults to "restricted" (read-only) when omitted or
// unrecognized, and "name" is derived from the URI when omitted.
package config

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"strings"
)

// Connection is a single parsed, named Postgres connection entry from
// POSTGRES_CONNECTIONS.
type Connection struct {
	Name       string
	URI        string
	AccessMode AccessMode
}

// rawConnection is the wire shape of a single POSTGRES_CONNECTIONS entry.
type rawConnection struct {
	Name       string `json:"name"`
	URI        string `json:"uri"`
	AccessMode string `json:"access_mode"`
}

// Parse parses values (the raw string of each -connection flag occurrence,
// in the order given) into connection entries. Every value must be a single
// non-empty JSON object with a non-empty "uri". "access_mode" defaults to
// "restricted" if omitted or unrecognized. "name" is derived from the URI
// (as "host/dbname") if omitted; derived or explicit names that collide
// with an earlier entry get a numeric suffix appended so every connection
// stays uniquely addressable.
func Parse(values []string) ([]Connection, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one -connection flag is required")
	}

	seen := make(map[string]int, len(values))
	conns := make([]Connection, 0, len(values))

	for i, v := range values {
		flagNum := i + 1 // 1-based, matching how a user counts flags on the command line

		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("-connection #%d is empty", flagNum)
		}

		var e rawConnection
		if err := json.Unmarshal([]byte(v), &e, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("-connection #%d is not a valid JSON object: %w", flagNum, err)
		}

		if strings.TrimSpace(e.URI) == "" {
			return nil, fmt.Errorf("-connection #%d is missing a \"uri\"", flagNum)
		}

		mode := AccessModeFromString(e.AccessMode)

		name := strings.TrimSpace(e.Name)
		if name == "" {
			derived, err := deriveName(e.URI)
			if err != nil {
				return nil, fmt.Errorf("-connection #%d: could not derive a name from its uri (%w); provide an explicit \"name\"", flagNum, err)
			}
			name = derived
		}
		name = dedupeName(name, seen)

		conns = append(conns, Connection{Name: name, URI: e.URI, AccessMode: mode})
	}

	return conns, nil
}

// deriveName builds a human-readable, credential-free identifier from a
// Postgres connection URI, of the form "host/dbname" (falling back to just
// "host" or just "dbname" if one is absent). It only supports URI-style
// DSNs (postgres://... or postgresql://...); keyword/value DSNs
// (host=... dbname=...) must supply an explicit "name".
func deriveName(rawURI string) (string, error) {
	if !strings.Contains(rawURI, "://") {
		return "", fmt.Errorf("automatic name derivation only supports postgres:// or postgresql:// URIs")
	}

	u, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("invalid uri: %w", err)
	}

	host := u.Hostname()
	db := strings.TrimPrefix(u.Path, "/")

	switch {
	case host != "" && db != "":
		return host + "/" + db, nil
	case db != "":
		return db, nil
	case host != "":
		return host, nil
	default:
		return "", fmt.Errorf("uri has neither a host nor a database name")
	}
}

// dedupeName returns name, or name with a numeric suffix appended if it has
// already been seen, tracking occurrence counts in seen.
func dedupeName(name string, seen map[string]int) string {
	seen[name]++
	if seen[name] == 1 {
		return name
	}
	return fmt.Sprintf("%s-%d", name, seen[name])
}
