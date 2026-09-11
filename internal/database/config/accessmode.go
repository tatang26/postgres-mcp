package config

import (
	"encoding/json/v2"
	"fmt"
)

const (
	// Restricted permits only read-only statements. This is the default access mode.
	Restricted AccessMode = iota
	// Unrestricted permits any statement, including writes and DDL.
	Unrestricted
)

// AccessMode controls whether write/DDL statements are permitted against
// the connected database.
type AccessMode int

func (m AccessMode) String() string {
	if m == Unrestricted {
		return "unrestricted"
	}
	return "restricted"
}

func (m AccessMode) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

func (m *AccessMode) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("access mode must be a string: %w", err)
	}

	switch s {
	case Unrestricted.String():
		*m = Unrestricted
	case Restricted.String():
		*m = Restricted
	default:
		return fmt.Errorf("invalid access mode %q", s)
	}

	return nil
}
