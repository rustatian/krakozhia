package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    config
	}{
		{
			name:    "all fields",
			content: `{"profile": "krakozhia", "region": "eu-central-1", "bucket": "memory", "index": "notes-other-512"}`,
			want:    config{Profile: "krakozhia", Region: "eu-central-1", Bucket: "memory", Index: "notes-other-512"},
		},
		{
			name:    "no index gives the index of the default model",
			content: `{"region": "eu-central-1", "bucket": "memory"}`,
			want:    config{Region: "eu-central-1", Bucket: "memory", Index: "notes-titan-v2-1024"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := loadConfig(path)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got != tt.want {
				t.Errorf("loadConfig:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "no region", content: `{"bucket": "memory"}`},
		{name: "no bucket", content: `{"region": "eu-central-1"}`},
		{name: "unknown field", content: `{"region": "eu-central-1", "bucket": "memory", "regoin": "x"}`},
		{name: "not JSON", content: `region = "eu-central-1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := loadConfig(path)
			if err == nil {
				t.Errorf("loadConfig: got %+v, want an error", got)
			}
		})
	}
}

func TestLoadConfigNoFile(t *testing.T) {
	got, err := loadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err == nil {
		t.Errorf("loadConfig: got %+v, want an error", got)
	}
}
