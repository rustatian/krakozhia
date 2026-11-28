// Command krakozhia provides the self-hosted knowledge base server and CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rustatian/krakozhia/internal/cli"
	"github.com/rustatian/krakozhia/internal/enrich"
	"github.com/rustatian/krakozhia/internal/githubsync"
	"github.com/rustatian/krakozhia/internal/inference"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
	"github.com/rustatian/krakozhia/internal/webui"
)

func main() {
	// Recover panics with exit code 1 so Claude Code hooks never exit with code 2.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "krakozhia: panic:", r)
			os.Exit(1)
		}
	}()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "krakozhia:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: krakozhia <serve|recall|search|sync|sync-chats|sync-github|save|show|list|bench|brief|eval>")
	}
	if args[0] == "serve" {
		return runServe()
	}
	return cli.Run(args, os.Stdin, os.Stdout, os.Stderr)
}

func runServe() error {
	dsn := os.Getenv("KRAKOZHIA_DB_DSN")
	token := os.Getenv("KRAKOZHIA_API_TOKEN")
	if dsn == "" || token == "" {
		return errors.New("KRAKOZHIA_DB_DSN and KRAKOZHIA_API_TOKEN are required")
	}

	listen := envOr("KRAKOZHIA_LISTEN", ":8080")
	infURL := envOr("KRAKOZHIA_INFERENCE_URL", "http://llama-server.inference.svc.cluster.local:8080")
	embedModel := envOr("KRAKOZHIA_EMBED_MODEL", "qwen3-embedding-0.6b")
	chatModel := envOr("KRAKOZHIA_CHAT_MODEL", "gemma-4-12b-it")
	rerankModel := envOr("KRAKOZHIA_RERANK_MODEL", "bge-reranker-v2-m3")
	infTimeout := 180 * time.Second
	if raw := os.Getenv("KRAKOZHIA_INFERENCE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid KRAKOZHIA_INFERENCE_TIMEOUT %q", raw)
		}
		infTimeout = d
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := st.ReclaimRunningJobs(ctx); err != nil {
		return fmt.Errorf("reclaim running jobs: %w", err)
	}

	inf := inference.New(inference.Config{
		BaseURL:     infURL,
		EmbedModel:  embedModel,
		ChatModel:   chatModel,
		RerankModel: rerankModel,
		Timeout:     infTimeout,
	})

	var ghSyncer *githubsync.Syncer
	if owners := githubsync.ParseOwners(os.Getenv("KRAKOZHIA_GITHUB_OWNERS")); len(owners) > 0 {
		ghToken := os.Getenv("KRAKOZHIA_GITHUB_TOKEN")
		ghTokens := githubsync.ParseOwnerTokens(os.Getenv("KRAKOZHIA_GITHUB_TOKENS"))
		if ghToken != "" || len(ghTokens) > 0 {
			syncInterval := 2 * time.Hour
			if raw := os.Getenv("KRAKOZHIA_GITHUB_SYNC_INTERVAL"); raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					return fmt.Errorf("invalid KRAKOZHIA_GITHUB_SYNC_INTERVAL %q", raw)
				}
				syncInterval = d
			}
			closedLookback := 90 * 24 * time.Hour
			if raw := os.Getenv("KRAKOZHIA_GITHUB_CLOSED_LOOKBACK_DAYS"); raw != "" {
				days, err := strconv.Atoi(raw)
				if err != nil || days <= 0 {
					return fmt.Errorf("invalid KRAKOZHIA_GITHUB_CLOSED_LOOKBACK_DAYS %q", raw)
				}
				closedLookback = time.Duration(days) * 24 * time.Hour
			}
			ghSyncer = githubsync.New(githubsync.Config{
				Store:          st,
				Owners:         owners,
				DefaultToken:   ghToken,
				OwnerTokens:    ghTokens,
				SyncInterval:   syncInterval,
				ClosedLookback: closedLookback,
				Logger:         slog.Default(),
			})
		}
	}

	// Wire read-only HTML UI using srv.Search so browser views stay consistent with API search.
	var srv *server.Server
	ui := webui.New(st, func(ctx context.Context, params webui.SearchParams) ([]store.SearchHit, bool, error) {
		res, err := srv.Search(ctx, server.SearchRequest{
			Form:       store.FormHybrid,
			Query:      params.Query,
			Project:    params.Project,
			Kind:       params.Kind,
			Limit:      20,
			DedupeDocs: true,
		})
		return res.Hits, res.Degraded, err
	})
	srv = server.New(server.Config{
		Store:        st,
		Inference:    inf,
		GitHubSyncer: ghSyncer,
		Token:        token,
		Version:      "0.1.0",
		WebUI:        ui,
	})

	workers := 1
	if raw := os.Getenv("KRAKOZHIA_ENRICH_WORKERS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid KRAKOZHIA_ENRICH_WORKERS %q", raw)
		}
		workers = n
	}

	var wg sync.WaitGroup
	for range workers {
		w := enrich.New(enrich.Config{
			Store:     st,
			Inference: inf,
			Logger:    slog.Default(),
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(ctx)
		}()
	}
	if ghSyncer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ghSyncer.Run(ctx)
		}()
	}

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "krakozhia serve listening on %s\n", listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := httpSrv.Shutdown(shutCtx)
		wg.Wait()
		return err
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
