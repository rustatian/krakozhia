package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/chats"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

const (
	chatIngestBatchSize = 10
	defaultChatSince    = "30d"
	defaultChatMinIdle  = 2 * time.Minute
)

func runSyncChats(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync-chats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceRaw := fs.String("since", defaultChatSince, "only include chats modified within this duration (for example 7d, 30d, 24h)")
	all := fs.Bool("all", false, "include all historical chats regardless of age")
	agent := fs.String("agent", "", "limit to one client: claude, antigravity, codex, or opencode")
	limit := fs.Int("limit", 0, "maximum new or changed chats to ingest (0 for unlimited)")
	dryRun := fs.Bool("dry-run", false, "list sessions that would be ingested without uploading")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := chats.ScanOptions{
		MinIdle: defaultChatMinIdle,
		Now:     time.Now().UTC(),
	}
	if !*all {
		dur, err := parseDurationWithDays(*sinceRaw)
		if err != nil {
			return fmt.Errorf("invalid --since %q: %w", *sinceRaw, err)
		}
		opts.Since = opts.Now.Add(-dur)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	projectsDir, err := ProjectsDir()
	if err != nil {
		return err
	}

	wanted := strings.ToLower(strings.TrimSpace(*agent))
	if wanted != "" && !slices.Contains([]string{"claude", "antigravity", "codex", "opencode"}, wanted) {
		return fmt.Errorf("unknown --agent %q (expected claude, antigravity, codex, or opencode)", wanted)
	}

	var discovered []chats.Session
	addScan := func(name string, fn func() ([]chats.Session, error)) {
		if wanted != "" && wanted != name {
			return
		}
		list, err := fn()
		if err != nil {
			fmt.Fprintf(stderr, "scan %s: %v\n", name, err)
			return
		}
		discovered = append(discovered, list...)
	}

	addScan("claude", func() ([]chats.Session, error) {
		return chats.ScanClaude(projectsDir, homeDir, opts)
	})
	addScan("antigravity", func() ([]chats.Session, error) {
		return chats.ScanAntigravity(filepath.Join(homeDir, ".gemini", "antigravity-cli"), homeDir, opts)
	})
	addScan("codex", func() ([]chats.Session, error) {
		return chats.ScanCodex(filepath.Join(homeDir, ".codex"), homeDir, opts)
	})
	addScan("opencode", func() ([]chats.Session, error) {
		return chats.ScanOpenCode(filepath.Join(homeDir, ".local", "share", "opencode"), homeDir, opts)
	})

	// Process newest conversations first so recent context lands fastest when backfilling.
	slices.SortFunc(discovered, func(a, b chats.Session) int {
		if a.UpdatedAt.After(b.UpdatedAt) {
			return -1
		}
		if a.UpdatedAt.Before(b.UpdatedAt) {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})

	if *dryRun {
		count := len(discovered)
		if *limit > 0 && count > *limit {
			count = *limit
		}
		for _, s := range discovered[:count] {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", s.ID, s.Project, s.UpdatedAt.UTC().Format(time.DateOnly), s.Title)
		}
		fmt.Fprintf(stdout, "sync-chats dry-run: %d sessions matched\n", count)
		return nil
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	client := NewClient(cfg.URL, cfg.Token)
	serverHashes, err := client.ListChatHashes(ctx)
	if err != nil {
		return err
	}

	var pending []store.IngestChatInput
	for _, s := range discovered {
		if serverHashes[s.ID] == s.TranscriptHash {
			continue
		}
		started := s.StartedAt
		updated := s.UpdatedAt
		pending = append(pending, store.IngestChatInput{
			ID:             s.ID,
			SourceAgent:    s.SourceAgent,
			ExternalID:     s.ExternalID,
			Project:        s.Project,
			WorkspacePath:  s.WorkspacePath,
			Title:          s.Title,
			TranscriptHash: s.TranscriptHash,
			RawExcerpt:     s.RawExcerpt,
			StartedAt:      &started,
			UpdatedAt:      &updated,
		})
		if *limit > 0 && len(pending) >= *limit {
			break
		}
	}

	if len(pending) == 0 {
		fmt.Fprintf(stdout, "sync-chats: up to date (%d sessions scanned)\n", len(discovered))
		return nil
	}

	totalQueued := 0
	totalUnchanged := 0
	totalErrors := 0
	for i := 0; i < len(pending); i += chatIngestBatchSize {
		end := min(i+chatIngestBatchSize, len(pending))
		res, err := client.IngestChats(ctx, server.IngestChatsRequest{
			Chats: pending[i:end],
		})
		if err != nil {
			return err
		}
		totalQueued += res.Queued
		totalUnchanged += res.Unchanged
		for _, e := range res.Errors {
			totalErrors++
			fmt.Fprintln(stderr, "sync-chats error:", e)
		}
	}
	fmt.Fprintf(stdout, "sync-chats: queued=%d unchanged=%d errors=%d scanned=%d\n",
		totalQueued, totalUnchanged, totalErrors, len(discovered))
	return nil
}

func parseDurationWithDays(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if before, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(before)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid day count %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
