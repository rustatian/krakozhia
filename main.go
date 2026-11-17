// Command krakozhia searches the Claude Code memory notes of all projects with Amazon S3 Vectors.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "krakozhia:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 || (args[0] != "init" && args[0] != "sync") {
		return errors.New("usage: krakozhia <init|sync>")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c, err := loadConfig(filepath.Join(home, ".config", "krakozhia", "config.json"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	br, sv, err := awsClients(ctx, c)
	if err != nil {
		return err
	}

	if args[0] == "init" {
		if err := createIndex(ctx, sv, c); err != nil {
			return err
		}
		fmt.Printf("created the index %s in the bucket %s\n", c.Index, c.Bucket)
		return nil
	}

	notes, errs := loadNotes(filepath.Join(home, ".claude", "projects"))
	written, deleted, syncErrs := syncNotes(ctx, br, sv, c, notes, filepath.Join(home, ".local", "state", "krakozhia", "manifest.json"))
	if written+deleted > 0 {
		fmt.Printf("wrote %d notes, deleted %d\n", written, deleted)
	}
	return errors.Join(append(errs, syncErrs...)...)
}
