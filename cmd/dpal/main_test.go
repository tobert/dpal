package main

import (
	"strings"
	"testing"
)

func emptyEnv(string) string { return "" }

func TestRun_NoKeyReturnsError(t *testing.T) {
	err := run([]string{}, emptyEnv)
	if err == nil {
		t.Fatal("expected error when no API key is provided, got nil")
	}
	if !strings.Contains(err.Error(), "API key required") {
		t.Errorf("error = %q, want it to mention 'API key required'", err.Error())
	}
}

func TestRun_VersionFlagExitsCleanly(t *testing.T) {
	// --version should not require an API key and should return nil.
	if err := run([]string{"--version"}, emptyEnv); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
}

func TestRun_BadFlagReturnsError(t *testing.T) {
	if err := run([]string{"--nope"}, emptyEnv); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}
