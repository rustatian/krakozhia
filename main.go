// Command krakozhia searches the Claude Code memory notes of all projects with Amazon S3 Vectors.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

func main() {
	// A Go panic exits with code 2, and code 2 from a UserPromptSubmit hook blocks the prompt.
	// https://code.claude.com/docs/en/hooks#exit-code-2-behavior-per-event
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "krakozhia: panic:", r)
			os.Exit(1)
		}
	}()
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "krakozhia:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || !slices.Contains([]string{"init", "sync", "recall", "search", "eval"}, args[0]) {
		return errors.New("usage: krakozhia <init|sync|recall|search|eval>")
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
	state := filepath.Join(home, ".local", "state", "krakozhia")

	switch args[0] {
	case "init":
		if err := createIndex(ctx, sv, c); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created the index %s in the bucket %s\n", c.Index, c.Bucket)
		return nil
	case "sync":
		removeOldSessions(filepath.Join(state, "sessions"))
		notes, errs := loadNotes(filepath.Join(home, ".claude", "projects"))
		written, deleted, syncErrs := syncNotes(ctx, br, sv, c, notes, filepath.Join(state, "manifest.json"))
		if written+deleted > 0 {
			fmt.Fprintf(stdout, "wrote %d notes, deleted %d\n", written, deleted)
		}
		return errors.Join(append(errs, syncErrs...)...)
	case "recall":
		// Claude Code stops the hook after 4 s. With this deadline, recall ends earlier with its own error line and a log entry.
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return recall(ctx, br, sv, c, home, stdin, stdout)
	case "eval":
		return eval(ctx, br, sv, c, args[1:], stdout)
	}
	return search(ctx, br, sv, c, home, args[1:], stdout)
}
