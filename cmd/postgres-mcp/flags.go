package main

import (
	"flag"
	"fmt"
	"strings"
)

// connectionFlag collects every occurrence of a repeated -connection flag,
// in the order they appear on the command line. It implements flag.Value.
type connectionFlag []string

func (c *connectionFlag) String() string {
	if c == nil {
		return ""
	}
	return strings.Join(*c, ", ")
}

func (c *connectionFlag) Set(value string) error {
	*c = append(*c, value)
	return nil
}

// parseArgs parses args (typically os.Args[1:]) and returns the raw value
// of every -connection flag, in the order given. Each value is expected to
// be a single JSON object describing one Postgres connection; see
// config.Parse for how they're interpreted. parseArgs itself does no JSON
// validation - it only extracts the flag values.
func parseArgs(args []string) ([]string, error) {
	fs := flag.NewFlagSet("postgres-mcp", flag.ContinueOnError)

	var connections connectionFlag
	fs.Var(&connections, "connection", `A JSON object describing one Postgres connection, e.g. -connection={"name":"foo","uri":"postgres://...","access_mode":"restricted"}. Repeat this flag once per connection; at least one is required.`)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument(s): %v", fs.Args())
	}

	return connections, nil
}
