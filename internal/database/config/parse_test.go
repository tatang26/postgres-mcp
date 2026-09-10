package config

import (
	"strings"
	"testing"
)

func TestParse_NoValues(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("expected error for no -connection flags")
	}
	if _, err := Parse([]string{}); err == nil {
		t.Fatal("expected error for no -connection flags")
	}
}

func TestParse_BlankValue(t *testing.T) {
	_, err := Parse([]string{"", "   "})
	if err == nil {
		t.Fatal("expected error for a blank -connection value")
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	if _, err := Parse([]string{"not json"}); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParse_ArrayValueRejected(t *testing.T) {
	// Each -connection flag must be a single JSON object, not an array of
	// them; a whole array should fail (it doesn't unmarshal into
	// rawConnection).
	_, err := Parse([]string{`[{"uri": "postgres://localhost/db"}]`})
	if err == nil {
		t.Fatal("expected error when a -connection value is a JSON array instead of an object")
	}
}

func TestParse_UnknownFieldRejected(t *testing.T) {
	_, err := Parse([]string{`{"uri": "postgres://localhost/db", "acess_mode": "unrestricted"}`})
	if err == nil {
		t.Fatal("expected error for an unrecognized field (typo)")
	}
}

func TestParse_MissingURI(t *testing.T) {
	_, err := Parse([]string{`{"name": "x"}`})
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
	if !strings.Contains(err.Error(), "uri") {
		t.Errorf("expected error to mention 'uri', got: %v", err)
	}
}

func TestParse_DefaultsToRestrictedWhenAccessModeOmitted(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres://localhost/db"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].AccessMode != Restricted {
		t.Errorf("expected Restricted, got %v", conns[0].AccessMode)
	}
}

func TestParse_UnrecognizedAccessModeDefaultsToRestricted(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres://localhost/db", "access_mode": "bogus"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].AccessMode != Restricted {
		t.Errorf("expected Restricted for unrecognized access_mode, got %v", conns[0].AccessMode)
	}
}

func TestParse_UnrestrictedAccessMode(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres://localhost/db", "access_mode": "unrestricted"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].AccessMode != Unrestricted {
		t.Errorf("expected Unrestricted, got %v", conns[0].AccessMode)
	}
}

func TestParse_ExplicitName(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres://localhost/db", "name": "my-conn"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].Name != "my-conn" {
		t.Errorf("expected name 'my-conn', got %q", conns[0].Name)
	}
}

func TestParse_DerivesNameFromHostAndDB(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres://user:pass@myhost:5432/mydb"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].Name != "myhost/mydb" {
		t.Errorf("expected name 'myhost/mydb', got %q", conns[0].Name)
	}
}

func TestParse_DerivesNameFromDBOnlyWhenNoHost(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "postgres:///mydb"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].Name != "mydb" {
		t.Errorf("expected name 'mydb', got %q", conns[0].Name)
	}
}

func TestParse_KeywordValueDSNWithoutNameFails(t *testing.T) {
	_, err := Parse([]string{`{"uri": "host=localhost dbname=mydb"}`})
	if err == nil {
		t.Fatal("expected error: keyword/value DSNs require an explicit name")
	}
}

func TestParse_KeywordValueDSNWithExplicitNameSucceeds(t *testing.T) {
	conns, err := Parse([]string{`{"uri": "host=localhost dbname=mydb", "name": "kv-conn"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[0].Name != "kv-conn" {
		t.Errorf("expected name 'kv-conn', got %q", conns[0].Name)
	}
}

func TestParse_DedupesCollidingNames(t *testing.T) {
	conns, err := Parse([]string{
		`{"uri": "postgres://myhost/mydb"}`,
		`{"uri": "postgres://myhost/mydb"}`,
		`{"uri": "postgres://myhost/mydb"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conns) != 3 {
		t.Fatalf("expected 3 connections, got %d", len(conns))
	}
	names := []string{conns[0].Name, conns[1].Name, conns[2].Name}
	want := []string{"myhost/mydb", "myhost/mydb-2", "myhost/mydb-3"}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("connection %d: expected name %q, got %q", i, want[i], n)
		}
	}
}

func TestParse_DedupesExplicitNameAgainstDerivedName(t *testing.T) {
	conns, err := Parse([]string{
		`{"uri": "postgres://myhost/mydb"}`,
		`{"uri": "postgres://other/other", "name": "myhost/mydb"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conns[1].Name != "myhost/mydb-2" {
		t.Errorf("expected second connection to be deduped to 'myhost/mydb-2', got %q", conns[1].Name)
	}
}

func TestParse_MultipleConnectionsPreserveOrderAndFields(t *testing.T) {
	conns, err := Parse([]string{
		`{"uri": "postgres://a/db1", "access_mode": "unrestricted", "name": "one"}`,
		`{"uri": "postgres://b/db2"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conns))
	}
	if conns[0].Name != "one" || conns[0].URI != "postgres://a/db1" || conns[0].AccessMode != Unrestricted {
		t.Errorf("unexpected first connection: %+v", conns[0])
	}
	if conns[1].Name != "b/db2" || conns[1].AccessMode != Restricted {
		t.Errorf("unexpected second connection: %+v", conns[1])
	}
}

func TestAccessModeFromString(t *testing.T) {
	if AccessModeFromString("unrestricted") != Unrestricted {
		t.Error("expected 'unrestricted' to map to Unrestricted")
	}
	if AccessModeFromString("restricted") != Restricted {
		t.Error("expected 'restricted' to map to Restricted")
	}
	if AccessModeFromString("") != Restricted {
		t.Error("expected empty string to default to Restricted")
	}
	if AccessModeFromString("garbage") != Restricted {
		t.Error("expected unrecognized value to default to Restricted")
	}
}

func TestAccessMode_String(t *testing.T) {
	if Restricted.String() != "restricted" {
		t.Errorf("expected 'restricted', got %q", Restricted.String())
	}
	if Unrestricted.String() != "unrestricted" {
		t.Errorf("expected 'unrestricted', got %q", Unrestricted.String())
	}
}
