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
	"bytes"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Connection is a single parsed, named Postgres connection entry from
// POSTGRES_CONNECTIONS.
type Connection struct {
	Name       string     `json:"name"`
	URI        string     `json:"uri"`
	AccessMode AccessMode `json:"access_mode"`
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

	conns := make([]Connection, 0, len(values))
	trimStringOption := json.WithUnmarshalers(json.UnmarshalFunc(func(b []byte, dst *string) error {
		*dst = string(bytes.TrimSpace(b))
		return nil
	}))

	for i, v := range values {
		var con Connection

		err := json.Unmarshal(
			[]byte(v), &con,
			trimStringOption,
			json.RejectUnknownMembers(true),
		)
		if err != nil {
			return nil, fmt.Errorf("-connection #%d is not a valid json object: %w", i+1, err)
		}

		if con.Name == "" {
			return nil, fmt.Errorf(`-connection #%d doesn't have a valid name.`, i+1)
		}

		if con.URI == "" {
			return nil, fmt.Errorf(`-connection #%d doesn't have a valid uri.`, i+1)
		}

		if slices.ContainsFunc(conns, func(c Connection) bool { return c.Name == con.Name }) {
			continue
		}

		conns = append(conns, con)
	}

	return conns, nil
}
