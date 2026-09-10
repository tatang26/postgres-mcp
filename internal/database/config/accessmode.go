package config

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

func AccessModeFromString(s string) AccessMode {
	if s == "unrestricted" {
		return Unrestricted
	}

	return Restricted
}
