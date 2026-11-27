package inference_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/inference"
)

func TestEmbedQueryAndDocument(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		isQuery   bool
		input     string
		wantInput string
		status    int
		respBody  string
		wantVec   []float32
		wantErr   bool
	}{
		{
			name:      "query adds instruction prefix",
			isQuery:   true,
			input:     "pgvector tuning",
			wantInput: inference.QueryInstruction + "pgvector tuning",
			status:    http.StatusOK,
			respBody:  `{"data":[{"embedding":[0.1,0.2,0.3]}]}`,
			wantVec:   []float32{0.1, 0.2, 0.3},
		},
		{
			name:      "document uses raw input",
			isQuery:   false,
			input:     "chunk text",
			wantInput: "chunk text",
			status:    http.StatusOK,
			respBody:  `{"data":[{"embedding":[0.4,0.5]}]}`,
			wantVec:   []float32{0.4, 0.5},
		},
		{
			name:     "non-200 status fails",
			isQuery:  true,
			input:    "fail",
			status:   http.StatusBadGateway,
			respBody: `upstream error`,
			wantErr:  true,
		},
		{
			name:     "empty embedding fails",
			isQuery:  false,
			input:    "empty",
			status:   http.StatusOK,
			respBody: `{"data":[{"embedding":[]}]}`,
			wantErr:  true,
		},
		{
			name:     "malformed json fails",
			isQuery:  false,
			input:    "bad",
			status:   http.StatusOK,
			respBody: `{not-json`,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var req struct {
					Model string `json:"model"`
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if req.Model != "qwen3-embedding-0.6b" {
					t.Fatalf("model = %q, want qwen3-embedding-0.6b", req.Model)
				}
				if tc.wantInput != "" && req.Input != tc.wantInput {
					t.Fatalf("input = %q, want %q", req.Input, tc.wantInput)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()

			c := inference.New(inference.Config{
				BaseURL:     srv.URL,
				EmbedModel:  "qwen3-embedding-0.6b",
				ChatModel:   "qwen3-4b-instruct",
				RerankModel: "bge-reranker-v2-m3",
				Timeout:     2 * time.Second,
			})

			var (
				got []float32
				err error
			)
			if tc.isQuery {
				got, err = c.EmbedQuery(context.Background(), tc.input)
			} else {
				got, err = c.EmbedDocument(context.Background(), tc.input)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.wantVec) {
				t.Fatalf("vec = %v, want %v", got, tc.wantVec)
			}
		})
	}
}

func TestChatAndRerankAndHealth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/chat/completions":
			var req struct {
				Model       string  `json:"model"`
				Temperature float64 `json:"temperature"`
				MaxTokens   int     `json:"max_tokens"`
				Messages    []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode chat: %v", err)
			}
			if req.Model != "qwen3-4b-instruct" || req.MaxTokens != 64 || req.Temperature != 0 || len(req.Messages) != 2 {
				t.Fatalf("unexpected chat payload: %+v", req)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  Summary line.  "}}]}`))
		case "/v1/rerank":
			var req struct {
				Model     string   `json:"model"`
				Query     string   `json:"query"`
				Documents []string `json:"documents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode rerank: %v", err)
			}
			if req.Model != "bge-reranker-v2-m3" || req.Query != "pgvector" || len(req.Documents) != 2 {
				t.Fatalf("unexpected rerank payload: %+v", req)
			}
			_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.12},{"index":0,"relevance_score":0.95}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := inference.New(inference.Config{
		BaseURL:     srv.URL,
		EmbedModel:  "qwen3-embedding-0.6b",
		ChatModel:   "qwen3-4b-instruct",
		RerankModel: "bge-reranker-v2-m3",
		Timeout:     2 * time.Second,
	})

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v", err)
	}
	reply, err := c.Chat(context.Background(), "sys", "usr", 64)
	if err != nil {
		t.Fatalf("Chat() = %v", err)
	}
	if reply != "Summary line." {
		t.Fatalf("Chat() = %q, want %q", reply, "Summary line.")
	}
	ranked, err := c.Rerank(context.Background(), "pgvector", []string{"doc a", "doc b"})
	if err != nil {
		t.Fatalf("Rerank() = %v", err)
	}
	wantRanked := []inference.RerankResult{{Index: 0, Score: 0.95}, {Index: 1, Score: 0.12}}
	if !slices.Equal(ranked, wantRanked) {
		t.Fatalf("Rerank() = %+v, want %+v", ranked, wantRanked)
	}
}

func TestClientTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := inference.New(inference.Config{
		BaseURL: srv.URL,
		Timeout: 30 * time.Millisecond,
	})
	err := c.Health(context.Background())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "deadline") && !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
