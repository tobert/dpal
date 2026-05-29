package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the TOML schema for ~/.config/dpal/config.toml.
//
// Three layering knobs, mirroring gpal:
//   - IncludeDefaultPrompt — whether to prepend the built-in DeepSeek
//     prompt (default true; suppress via flag or config).
//   - SystemPrompt — inline prompt text.
//   - SystemPrompts — paths to additional prompt files, appended in order.
//
// CLI --system-prompt files always layer on top of these, regardless of
// the config's content.
type Config struct {
	IncludeDefaultPrompt *bool    `toml:"include_default_prompt"`
	SystemPrompt         string   `toml:"system_prompt"`
	SystemPrompts        []string `toml:"system_prompts"`
}

// stringSliceFlag is a flag.Value that accumulates -repeated values
// instead of overwriting on each occurrence.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// loadConfig reads ~/.config/dpal/config.toml (or $XDG_CONFIG_HOME/dpal/...).
// A missing file is not an error; an invalid file logs a warning and
// returns the zero Config so dpal still starts.
func loadConfig(explicit string, getenv func(string) string) Config {
	path := explicit
	if path == "" {
		home := getenv("XDG_CONFIG_HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return Config{}
			}
			home = filepath.Join(h, ".config")
		}
		path = filepath.Join(home, "dpal", "config.toml")
	}

	if _, err := os.Stat(path); err != nil {
		if explicit != "" {
			log.Printf("dpal: --config %q: %v", path, err)
		}
		return Config{}
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		log.Printf("dpal: invalid TOML in %s: %v", path, err)
		return Config{}
	}
	return cfg
}

// composeSystemPrompt layers the prompt sources in fixed order:
//  1. Built-in default (unless suppressed)
//  2. Config inline system_prompt
//  3. Config system_prompts files
//  4. CLI --system-prompt files
//
// noDefault wins over the config's include_default_prompt setting.
// Empty result means no system message will be sent.
func composeSystemPrompt(cfg Config, cliFiles []string, noDefault bool) (string, error) {
	var parts []string

	includeDefault := true
	if cfg.IncludeDefaultPrompt != nil {
		includeDefault = *cfg.IncludeDefaultPrompt
	}
	if noDefault {
		includeDefault = false
	}
	if includeDefault {
		parts = append(parts, defaultSystemPrompt)
	}

	if s := strings.TrimSpace(cfg.SystemPrompt); s != "" {
		parts = append(parts, s)
	}
	for _, p := range cfg.SystemPrompts {
		body, err := readPromptFile(p)
		if err != nil {
			return "", fmt.Errorf("config system_prompts[%s]: %w", p, err)
		}
		if body != "" {
			parts = append(parts, body)
		}
	}
	for _, p := range cliFiles {
		body, err := readPromptFile(p)
		if err != nil {
			return "", fmt.Errorf("--system-prompt %s: %w", p, err)
		}
		if body != "" {
			parts = append(parts, body)
		}
	}

	return strings.Join(parts, "\n\n"), nil
}

func readPromptFile(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	path = os.ExpandEnv(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
