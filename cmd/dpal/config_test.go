package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The default system prompt must not describe file-exploration tools. It is
// reused for the disable_explore / oneshot synth call, which runs with no
// tools — and a prompt that advertises read_file to a tools=nil call primes
// the raw-tool-call leak (markup lands in content). Tool availability is
// communicated per-call by the server's conditional note, never the persona.
func TestDefaultSystemPrompt_DescribesNoTools(t *testing.T) {
	for _, tool := range []string{"read_file", "read_files", "search_project", "project_tree", "list_directory"} {
		if strings.Contains(defaultSystemPrompt, tool) {
			t.Errorf("default system prompt names the tool %q; it must stay tool-free (see the leak post-mortem)", tool)
		}
	}
}

func TestLoadConfig_MissingFileIsNotAnError(t *testing.T) {
	// No explicit path + a fake home that has no dpal config file.
	tmp := t.TempDir()
	cfg := loadConfig("", envWith("XDG_CONFIG_HOME", tmp))
	if cfg.SystemPrompt != "" || len(cfg.SystemPrompts) != 0 {
		t.Errorf("missing file should yield zero config, got %+v", cfg)
	}
}

func TestLoadConfig_ParsesTOML(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "dpal")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
include_default_prompt = false
system_prompt = "inline shaping"
system_prompts = ["/etc/never-read-this"]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig("", envWith("XDG_CONFIG_HOME", tmp))
	if cfg.IncludeDefaultPrompt == nil || *cfg.IncludeDefaultPrompt {
		t.Errorf("include_default_prompt should be false, got %v", cfg.IncludeDefaultPrompt)
	}
	if cfg.SystemPrompt != "inline shaping" {
		t.Errorf("system_prompt = %q", cfg.SystemPrompt)
	}
	if len(cfg.SystemPrompts) != 1 || cfg.SystemPrompts[0] != "/etc/never-read-this" {
		t.Errorf("system_prompts = %v", cfg.SystemPrompts)
	}
}

func TestLoadConfig_InvalidTOMLReturnsZero(t *testing.T) {
	// Malformed file: log a warning but don't crash.
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "dpal")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("not = valid = toml"), 0o600)

	cfg := loadConfig("", envWith("XDG_CONFIG_HOME", tmp))
	if cfg.SystemPrompt != "" {
		t.Errorf("expected zero config on parse error, got %+v", cfg)
	}
}

func TestComposeSystemPrompt_DefaultOnly(t *testing.T) {
	got, err := composeSystemPrompt(Config{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("expected default prompt, got empty")
	}
	if got != defaultSystemPrompt {
		t.Errorf("expected exactly the default; got %q", got)
	}
}

func TestComposeSystemPrompt_NoDefaultSuppressesBuiltIn(t *testing.T) {
	got, err := composeSystemPrompt(Config{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty when no default and nothing else; got %q", got)
	}
}

func TestComposeSystemPrompt_ConfigIncludeDefaultFalse(t *testing.T) {
	f := false
	got, err := composeSystemPrompt(Config{
		IncludeDefaultPrompt: &f,
		SystemPrompt:         "just this",
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "just this" {
		t.Errorf("got %q, want 'just this'", got)
	}
}

func TestComposeSystemPrompt_LayeringOrder(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "from-config.txt")
	cliFile := filepath.Join(tmp, "from-cli.txt")
	_ = os.WriteFile(cfgFile, []byte("CONFIG-FILE"), 0o600)
	_ = os.WriteFile(cliFile, []byte("CLI-FILE"), 0o600)

	got, err := composeSystemPrompt(Config{
		SystemPrompt:  "CONFIG-INLINE",
		SystemPrompts: []string{cfgFile},
	}, []string{cliFile}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Order: default, then config inline, then config files, then CLI files.
	wantOrder := []string{defaultSystemPrompt, "CONFIG-INLINE", "CONFIG-FILE", "CLI-FILE"}
	for i := 1; i < len(wantOrder); i++ {
		if strings.Index(got, wantOrder[i-1]) > strings.Index(got, wantOrder[i]) {
			t.Errorf("layering order broken: %q must precede %q in:\n%s",
				wantOrder[i-1][:min(40, len(wantOrder[i-1]))], wantOrder[i], got)
		}
	}
}

func TestComposeSystemPrompt_MissingFileReturnsError(t *testing.T) {
	_, err := composeSystemPrompt(Config{
		SystemPrompts: []string{"/definitely/not/a/real/path"},
	}, nil, true)
	if err == nil {
		t.Fatal("expected error for missing config system_prompts file")
	}
}

func TestStringSliceFlag_Accumulates(t *testing.T) {
	var s stringSliceFlag
	_ = s.Set("a")
	_ = s.Set("b")
	_ = s.Set("c")
	if len(s) != 3 || s[0] != "a" || s[1] != "b" || s[2] != "c" {
		t.Errorf("got %v, want [a b c]", []string(s))
	}
}
