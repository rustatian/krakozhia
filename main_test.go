package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestClaudePluginManifests(t *testing.T) {
	t.Parallel()

	rawPlugin, err := os.ReadFile(".claude-plugin/plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var plugin struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rawPlugin, &plugin); err != nil {
		t.Fatalf("unmarshal plugin.json: %v", err)
	}
	if plugin.Name != "krakozhia" || plugin.Description == "" {
		t.Fatalf("unexpected plugin.json: %+v", plugin)
	}

	rawMarket, err := os.ReadFile(".claude-plugin/marketplace.json")
	if err != nil {
		t.Fatalf("read marketplace.json: %v", err)
	}
	var market struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(rawMarket, &market); err != nil {
		t.Fatalf("unmarshal marketplace.json: %v", err)
	}
	if market.Name != "krakozhia" || len(market.Plugins) != 1 || market.Plugins[0].Name != "krakozhia" {
		t.Fatalf("unexpected marketplace.json: %+v", market)
	}

	rawHooks, err := os.ReadFile("hooks/hooks.json")
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
				Timeout int      `json:"timeout"`
				Async   bool     `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(rawHooks, &hooks); err != nil {
		t.Fatalf("unmarshal hooks.json: %v", err)
	}
	if len(hooks.Hooks["UserPromptSubmit"]) == 0 || len(hooks.Hooks["Stop"]) == 0 {
		t.Fatalf("expected UserPromptSubmit and Stop hooks: %+v", hooks.Hooks)
	}
}
