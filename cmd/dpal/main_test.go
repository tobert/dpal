package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func emptyEnv(string) string { return "" }

func envWith(k, v string) func(string) string {
	return func(s string) string {
		if s == k {
			return v
		}
		return ""
	}
}

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
	if err := run([]string{"--version"}, emptyEnv); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
}

func TestRun_BadFlagReturnsError(t *testing.T) {
	if err := run([]string{"--nope"}, emptyEnv); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}

func TestRun_RejectsBothApiKeyFlags(t *testing.T) {
	err := run([]string{"--api-key", "x", "--api-key-file", "/tmp/key"}, emptyEnv)
	if err == nil {
		t.Fatal("expected error when both --api-key and --api-key-file are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want 'mutually exclusive'", err.Error())
	}
}

func TestRun_ApiKeyFileMustExist(t *testing.T) {
	err := run([]string{"--api-key-file", "/nope/does/not/exist"}, emptyEnv)
	if err == nil {
		t.Fatal("expected error for missing key file")
	}
	if !strings.Contains(err.Error(), "api-key-file") {
		t.Errorf("error = %q, want it to mention --api-key-file", err.Error())
	}
}

func TestResolveAPIKey_FlagWins(t *testing.T) {
	got, err := resolveAPIKey("from-flag", "", envWith("DEEPSEEK_API_KEY", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-flag" {
		t.Errorf("got %q, want from-flag", got)
	}
}

func TestResolveAPIKey_FileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAPIKey("", keyPath, envWith("DEEPSEEK_API_KEY", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-file" {
		t.Errorf("got %q, want from-file (trimmed)", got)
	}
}

func TestResolveAPIKey_TrimsWhitespaceAndNewlines(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("  sk-abc-123\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAPIKey("", keyPath, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-abc-123" {
		t.Errorf("got %q, want %q", got, "sk-abc-123")
	}
}

func TestResolveAPIKey_EmptyFileErrors(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("   \n  "), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAPIKey("", keyPath, emptyEnv)
	if err == nil {
		t.Fatal("expected error for empty key file")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to mention 'empty'", err.Error())
	}
}

func TestResolveAPIKey_EnvFallback(t *testing.T) {
	got, err := resolveAPIKey("", "", envWith("DEEPSEEK_API_KEY", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want from-env", got)
	}
}

func TestResolveAPIKey_AllEmptyErrors(t *testing.T) {
	_, err := resolveAPIKey("", "", emptyEnv)
	if err == nil {
		t.Fatal("expected error when no source provides a key")
	}
}

func TestResolveOTelEndpoint_FlagWins(t *testing.T) {
	got, err := resolveOTelEndpoint("from-flag", "", envWith("OTEL_EXPORTER_OTLP_ENDPOINT", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-flag" {
		t.Errorf("got %q, want from-flag", got)
	}
}

func TestResolveOTelEndpoint_FileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "endpoint")
	if err := os.WriteFile(path, []byte("127.0.0.1:46861\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveOTelEndpoint("", path, envWith("OTEL_EXPORTER_OTLP_ENDPOINT", "from-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:46861" {
		t.Errorf("got %q, want trimmed file contents", got)
	}
}

func TestResolveOTelEndpoint_RejectsBothFlagAndFile(t *testing.T) {
	_, err := resolveOTelEndpoint("from-flag", "/some/path", emptyEnv)
	if err == nil {
		t.Fatal("expected error when both flag and file set")
	}
}

func TestResolveOTelEndpoint_EmptyFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "endpoint")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOTelEndpoint("", path, emptyEnv); err == nil {
		t.Fatal("expected error for empty endpoint file")
	}
}

func TestResolveOTelEndpoint_EnvFallbacks(t *testing.T) {
	got, err := resolveOTelEndpoint("", "", envWith("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "traces-env"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "traces-env" {
		t.Errorf("got %q, want traces-env (second env fallback)", got)
	}
}

func TestResolveOTelEndpoint_AllEmptyReturnsEmpty(t *testing.T) {
	got, err := resolveOTelEndpoint("", "", emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (otel is opt-in)", got)
	}
}
