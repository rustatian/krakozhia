// Package inference calls an OpenAI-compatible llama-server endpoint for embeddings, chat, and reranking.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// QueryInstruction is prepended to search queries for Qwen3-Embedding models.
const QueryInstruction = "Instruct: Given a search query, retrieve relevant technical documents, memory notes, and benchmark records\nQuery: "

// Config configures the inference HTTP client.
type Config struct {
	BaseURL     string
	EmbedModel  string
	ChatModel   string
	RerankModel string
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// RerankResult holds one scored candidate index returned by /v1/rerank.
type RerankResult struct {
	Index int
	Score float64
}

// Client calls llama-server endpoints.
type Client struct {
	baseURL     string
	embedModel  string
	chatModel   string
	rerankModel string
	timeout     time.Duration
	http        *http.Client
}

// New returns a configured inference Client.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		embedModel:  cfg.EmbedModel,
		chatModel:   cfg.ChatModel,
		rerankModel: cfg.RerankModel,
		timeout:     timeout,
		http:        hc,
	}
}

// Health checks GET /health on the inference server.
func (c *Client) Health(ctx context.Context) error {
	var resp struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, &resp); err != nil {
		return err
	}
	if resp.Status != "" && resp.Status != "ok" {
		return fmt.Errorf("inference health status %q", resp.Status)
	}
	return nil
}

// EmbedQuery embeds a search query with the Qwen3 query instruction prefix.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, QueryInstruction+text)
}

// EmbedDocument embeds a document chunk or memory note without a query prefix.
func (c *Client) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, text)
}

func (c *Client) embed(ctx context.Context, input string) ([]float32, error) {
	if c.embedModel == "" {
		return nil, errors.New("embed model is not configured")
	}
	req := struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{Model: c.embedModel, Input: input}

	var resp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/embeddings", req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, errors.New("inference returned empty embedding")
	}
	return resp.Data[0].Embedding, nil
}

// Chat sends a deterministic chat completion request and returns the trimmed reply text.
func (c *Client) Chat(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error) {
	if c.chatModel == "" {
		return "", errors.New("chat model is not configured")
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	req := struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		MaxTokens   int     `json:"max_tokens,omitempty"`
		Messages    []msg   `json:"messages"`
	}{
		Model:       c.chatModel,
		Temperature: 0,
		MaxTokens:   maxTokens,
		Messages: []msg{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/chat/completions", req, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("inference returned no chat choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// Rerank scores documents against query via POST /v1/rerank and sorts highest score first.
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error) {
	if c.rerankModel == "" {
		return nil, errors.New("rerank model is not configured")
	}
	if len(docs) == 0 {
		return nil, nil
	}
	req := struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}{Model: c.rerankModel, Query: query, Documents: docs}

	var resp struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/rerank", req, &resp); err != nil {
		return nil, err
	}
	out := make([]RerankResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, RerankResult{Index: r.Index, Score: r.RelevanceScore})
	}
	slices.SortFunc(out, func(a, b RerankResult) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return a.Index - b.Index
	})
	return out, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	if c.baseURL == "" {
		return errors.New("inference base URL is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("inference %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("inference %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}
