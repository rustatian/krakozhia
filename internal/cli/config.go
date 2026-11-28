// Package cli implements the thin krakozhia client commands.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Default configuration values for krakozhia CLI clients.
const (
	DefaultURL          = "https://kb.lab.rustatian.me"
	DefaultMaxDistance  = 0.42
	DefaultMinTextScore = 1.50
	DefaultMaxPointers  = 3
	DefaultSourceAgent  = "claude"
)

// Config configures the krakozhia CLI HTTP client and recall gate.
type Config struct {
	URL          string  `json:"url"`
	Token        string  `json:"token"`
	MaxDistance  float64 `json:"max_distance"`
	MinTextScore float64 `json:"min_text_score"`
	MaxPointers  int     `json:"max_pointers"`
	SourceAgent  string  `json:"source_agent"`
}

func defaultConfig() Config {
	return Config{
		URL:          DefaultURL,
		MaxDistance:  DefaultMaxDistance,
		MinTextScore: DefaultMinTextScore,
		MaxPointers:  DefaultMaxPointers,
		SourceAgent:  DefaultSourceAgent,
	}
}

// ConfigPath returns the path to the CLI configuration file.
func ConfigPath() (string, error) {
	if p := os.Getenv("KRAKOZHIA_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "krakozhia", "config.json"), nil
}

// LoadConfig reads ~/.config/krakozhia/config.json, applies defaults, and validates fields.
func LoadConfig() (Config, error) {
	cfg := defaultConfig()
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && os.Getenv("KRAKOZHIA_API_TOKEN") != "" {
			cfg.Token = os.Getenv("KRAKOZHIA_API_TOKEN")
			return cfg, validateConfig(cfg)
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode config %s: trailing data", path)
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("KRAKOZHIA_API_TOKEN")
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.URL == "" {
		return errors.New("config url is required")
	}
	if cfg.Token == "" {
		return errors.New("config token or KRAKOZHIA_API_TOKEN is required")
	}
	if cfg.MaxPointers <= 0 {
		return errors.New("config max_pointers must be positive")
	}
	if cfg.MaxDistance <= 0 {
		return errors.New("config max_distance must be positive")
	}
	if cfg.MinTextScore < 0 {
		return errors.New("config min_text_score must be non-negative")
	}
	return nil
}

// StateDir returns the local CLI state directory (~/.local/state/krakozhia).
func StateDir() (string, error) {
	if p := os.Getenv("KRAKOZHIA_STATE_DIR"); p != "" {
		return p, nil
	}
	if p := os.Getenv("XDG_STATE_HOME"); p != "" {
		return filepath.Join(p, "krakozhia"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "krakozhia"), nil
}

// ProjectsDir returns the Claude Code projects root (~/.claude/projects).
func ProjectsDir() (string, error) {
	if p := os.Getenv("KRAKOZHIA_PROJECTS_DIR"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}
