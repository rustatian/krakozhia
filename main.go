// Command krakozhia provides the self-hosted knowledge base server and CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rustatian/krakozhia/internal/inference"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
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
		return errors.New("usage: krakozhia <serve>")
	}
	switch args[0] {
	case "serve":
		return runServe()
	default:
		return fmt.Errorf("unknown command %q: usage: krakozhia <serve>", args[0])
	}
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
	chatModel := envOr("KRAKOZHIA_CHAT_MODEL", "qwen3-4b-instruct")
	rerankModel := envOr("KRAKOZHIA_RERANK_MODEL", "bge-reranker-v2-m3")

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

	inf := inference.New(inference.Config{
		BaseURL:     infURL,
		EmbedModel:  embedModel,
		ChatModel:   chatModel,
		RerankModel: rerankModel,
		Timeout:     30 * time.Second,
	})

	srv := server.New(server.Config{
		Store:     st,
		Inference: inf,
		Token:     token,
		Version:   "0.1.0",
	})

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
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
