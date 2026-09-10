package main

import (
	"strings"
	"testing"
	"time"
)

func TestWithDefaultSSLMode_URI_AddsPreferWhenAbsent(t *testing.T) {
	got, err := withDefaultSSLMode("postgres://user:pass@localhost:5432/mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=prefer") {
		t.Errorf("expected sslmode=prefer to be added, got: %s", got)
	}
}

func TestWithDefaultSSLMode_URI_RemoteHostDefaultsToRequire(t *testing.T) {
	got, err := withDefaultSSLMode("postgres://user:pass@prodcf.example.com:5432/mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("expected sslmode=require for a remote host, got: %s", got)
	}
}

func TestWithDefaultSSLMode_URI_LoopbackVariantsDefaultToPrefer(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		got, err := withDefaultSSLMode("postgres://user:pass@" + host + ":5432/mydb")
		if err != nil {
			t.Fatalf("unexpected error for host %s: %v", host, err)
		}
		if !strings.Contains(got, "sslmode=prefer") {
			t.Errorf("expected sslmode=prefer for loopback host %s, got: %s", host, got)
		}
	}
}

func TestWithDefaultSSLMode_URI_PreservesExistingSSLMode(t *testing.T) {
	got, err := withDefaultSSLMode("postgres://user:pass@localhost:5432/mydb?sslmode=disable")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("expected existing sslmode=disable to be preserved, got: %s", got)
	}
	if strings.Contains(got, "sslmode=prefer") {
		t.Errorf("expected sslmode not to be overwritten, got: %s", got)
	}
}

func TestWithDefaultSSLMode_URI_PreservesOtherQueryParams(t *testing.T) {
	got, err := withDefaultSSLMode("postgres://user:pass@localhost:5432/mydb?connect_timeout=5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "connect_timeout=5") {
		t.Errorf("expected connect_timeout to be preserved, got: %s", got)
	}
	if !strings.Contains(got, "sslmode=prefer") {
		t.Errorf("expected sslmode=prefer to be added, got: %s", got)
	}
}

func TestWithDefaultSSLMode_URI_InvalidURI(t *testing.T) {
	_, err := withDefaultSSLMode("postgres://user:%zz@localhost/mydb")
	if err == nil {
		t.Fatal("expected error for malformed URI")
	}
}

func TestWithDefaultSSLMode_KeywordValue_AddsPreferWhenAbsent(t *testing.T) {
	got, err := withDefaultSSLMode("host=localhost port=5432 user=postgres dbname=mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=prefer") {
		t.Errorf("expected sslmode=prefer to be added, got: %s", got)
	}
}

func TestWithDefaultSSLMode_KeywordValue_PreservesExistingSSLMode(t *testing.T) {
	got, err := withDefaultSSLMode("host=localhost dbname=mydb sslmode=verify-full")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "host=localhost dbname=mydb sslmode=verify-full" {
		t.Errorf("expected dsn to be unchanged, got: %s", got)
	}
}

func TestWithDefaultSSLMode_KeywordValue_EmptyDSN(t *testing.T) {
	got, err := withDefaultSSLMode("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty dsn to remain empty, got: %q", got)
	}
}

func TestWithDefaultSSLMode_KeywordValue_RemoteHostDefaultsToRequire(t *testing.T) {
	got, err := withDefaultSSLMode("host=prodcf.example.com dbname=mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("expected sslmode=require for a remote host, got: %s", got)
	}
}

func TestWithDefaultSSLMode_KeywordValue_MissingHostDefaultsToPrefer(t *testing.T) {
	got, err := withDefaultSSLMode("dbname=mydb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "sslmode=prefer") {
		t.Errorf("expected sslmode=prefer when host is omitted (defaults to local socket), got: %s", got)
	}
}

func TestWithStatementTimeout_URI_AddsOptionsWhenAbsent(t *testing.T) {
	got, err := withStatementTimeout("postgres://user:pass@localhost:5432/mydb", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "statement_timeout%3D30000") && !strings.Contains(got, "statement_timeout=30000") {
		t.Errorf("expected a statement_timeout=30000 option to be added, got: %s", got)
	}
}

func TestWithStatementTimeout_URI_PreservesExistingOptions(t *testing.T) {
	dsn := "postgres://user:pass@localhost:5432/mydb?options=-c%20search_path%3Dpublic"
	got, err := withStatementTimeout(dsn, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "search_path") {
		t.Errorf("expected existing options to be preserved, got: %s", got)
	}
	if strings.Contains(got, "statement_timeout") {
		t.Errorf("expected statement_timeout not to be added over existing options, got: %s", got)
	}
}

func TestWithStatementTimeout_KeywordValue_AddsOptionsWhenAbsent(t *testing.T) {
	got, err := withStatementTimeout("host=localhost dbname=mydb", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "statement_timeout=30000") {
		t.Errorf("expected statement_timeout=30000 to be added, got: %s", got)
	}
}

func TestWithStatementTimeout_KeywordValue_PreservesExistingOptions(t *testing.T) {
	dsn := "host=localhost dbname=mydb options='-c search_path=public'"
	got, err := withStatementTimeout(dsn, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dsn {
		t.Errorf("expected dsn to be unchanged, got: %s", got)
	}
}
