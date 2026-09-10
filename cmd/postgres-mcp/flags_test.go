package main

import (
	"reflect"
	"testing"
)

func TestConnectionFlag_String(t *testing.T) {
	var nilPtr *connectionFlag
	if got := nilPtr.String(); got != "" {
		t.Errorf("expected empty string for nil *connectionFlag, got %q", got)
	}

	c := connectionFlag{`{"uri":"a"}`, `{"uri":"b"}`}
	want := `{"uri":"a"}, {"uri":"b"}`
	if got := c.String(); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestParseArgs_NoFlags(t *testing.T) {
	values, err := parseArgs(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("expected no values, got %v", values)
	}
}

func TestParseArgs_SingleConnectionEqualsForm(t *testing.T) {
	values, err := parseArgs([]string{`-connection={"uri":"postgres://localhost/db"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{`{"uri":"postgres://localhost/db"}`}
	if !reflect.DeepEqual(values, []string(want)) {
		t.Errorf("expected %v, got %v", want, values)
	}
}

func TestParseArgs_SingleConnectionSpaceForm(t *testing.T) {
	values, err := parseArgs([]string{"-connection", `{"uri":"postgres://localhost/db"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 1 || values[0] != `{"uri":"postgres://localhost/db"}` {
		t.Errorf("unexpected values: %v", values)
	}
}

func TestParseArgs_RepeatedFlagsPreserveOrder(t *testing.T) {
	values, err := parseArgs([]string{
		`-connection={"name":"a","uri":"postgres://localhost/a"}`,
		`-connection={"name":"b","uri":"postgres://localhost/b"}`,
		`-connection={"name":"c","uri":"postgres://localhost/c"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		`{"name":"a","uri":"postgres://localhost/a"}`,
		`{"name":"b","uri":"postgres://localhost/b"}`,
		`{"name":"c","uri":"postgres://localhost/c"}`,
	}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("expected %v, got %v", want, values)
	}
}

func TestParseArgs_UnknownFlagFails(t *testing.T) {
	if _, err := parseArgs([]string{"-bogus=1"}); err == nil {
		t.Fatal("expected error for an unrecognized flag")
	}
}

func TestParseArgs_UnexpectedPositionalArgFails(t *testing.T) {
	_, err := parseArgs([]string{
		`-connection={"uri":"postgres://localhost/db"}`,
		"extra-arg",
	})
	if err == nil {
		t.Fatal("expected error for an unexpected positional argument")
	}
}
