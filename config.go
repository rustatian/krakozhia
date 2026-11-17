package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// config is the content of ~/.config/krakozhia/config.json.
type config struct {
	Profile string `json:"profile"` // AWS CLI profile. Empty selects the default credential chain.
	Region  string `json:"region"`
	Bucket  string `json:"bucket"`
	Index   string `json:"index"`
}

func loadConfig(path string) (config, error) {
	f, err := os.Open(path)
	if err != nil {
		return config{}, err
	}
	defer f.Close()

	// The index name holds the embedding model and the dimension.
	c := config{Index: "notes-titan-v2-1024"}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Region == "" || c.Bucket == "" {
		return config{}, errors.New(path + ": region and bucket are necessary")
	}
	return c, nil
}
