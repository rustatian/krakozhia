# krakozhia

Self-hosted multi-model knowledge base backed by PostgreSQL with `pgvector` and local AMD GPU inference (`llama-server`).

## Architecture

`krakozhia serve` runs one Go HTTP server that stores versioned Markdown documents, heading-split chunks, Claude Code memory notes, structured benchmark rows, and background GPU enrichment jobs in PostgreSQL (`pgvector`). Vector embeddings (`qwen3-embedding-0.6b`), document/note summaries, chat distillation, roadmaps, and briefs (`gemma-4-12b-it`), and optional cross-encoder reranking (`bge-reranker-v2-m3`) run on `llama-server` over HTTP. Hybrid retrieval runs cosine similarity and bilingual (`english` + `simple`) PostgreSQL full-text search, fused with reciprocal rank fusion (`k=60`) and weighted by recency and supersede status.

Clients interact with `krakozhia` through three interfaces:

- Streamable HTTP MCP endpoint (`/mcp`) with bearer-token authentication for Claude Code, Antigravity/Gemini, Codex, and OpenCode.
- JSON REST API (`/api/v1/*`) used by the thin `krakozhia` CLI binary and hooks.
- Read-only HTML UI (`/`, `/roadmaps`, `/documents`, `/notes`, `/search`, `/projects/{project}`, `/document`, `/note`, `/benchmarks`) and Prometheus metrics (`/metrics`).

## CLI Installation and Configuration

Build and install the CLI binary onto `~/.local/bin` (on non-interactive hook `PATH`):

```sh
GOBIN=$HOME/.local/bin go install .
```

Create `~/.config/krakozhia/config.json` with mode `0600`:

```json
{
  "url": "https://kb.lab.rustatian.me",
  "token": "<bearer-token>",
  "max_distance": 0.42,
  "min_text_score": 0.75,
  "max_pointers": 3,
  "source_agent": "claude"
}
```

You can also provide the bearer token with `KRAKOZHIA_API_TOKEN` instead of storing it in `config.json`.

## CLI Commands

- `krakozhia serve`: Start the HTTP server, MCP handler, read-only web UI, Prometheus `/metrics`, background GPU enrichment workers, and optional GitHub roadmap syncer. Reads `KRAKOZHIA_DB_DSN`, `KRAKOZHIA_API_TOKEN`, `KRAKOZHIA_LISTEN` (default `:8080`), `KRAKOZHIA_INFERENCE_URL`, `KRAKOZHIA_INFERENCE_TIMEOUT` (default `180s`), `KRAKOZHIA_EMBED_MODEL`, `KRAKOZHIA_CHAT_MODEL` (default `gemma-4-12b-it`), `KRAKOZHIA_RERANK_MODEL`, `KRAKOZHIA_ENRICH_WORKERS` (default `1`), `KRAKOZHIA_GITHUB_OWNERS`, `KRAKOZHIA_GITHUB_TOKEN`, `KRAKOZHIA_GITHUB_TOKENS` (`owner=token` pairs), `KRAKOZHIA_GITHUB_SYNC_INTERVAL` (default `2h`), and `KRAKOZHIA_GITHUB_CLOSED_LOOKBACK_DAYS` (default `90`).
- `krakozhia recall`: Read Claude Code `UserPromptSubmit` JSON from stdin, skip empty prompts, slash commands, and prompts shorter than 15 characters, query `/api/v1/search` with a 3-second deadline, apply gate thresholds and per-session deduplication, and print at most 3 plain-text pointers. Never exits with code 2.
- `krakozhia search [--project P] [--kind note|doc|github] [--type T] [--tag G] [-k N] [--rerank] <query>`: Run ungated hybrid search and print matching pointers with snippets.
- `krakozhia sync`: Prune session files older than 7 days, scan `~/.claude/projects/*/memory/*.md`, resolve canonical project names, diff SHA-256 hashes against `/api/v1/notes/hashes`, and upload new or modified notes (plus locally deleted manifest IDs) via `/api/v1/notes/sync`.
- `krakozhia sync-chats [--since 30d] [--all] [--agent claude|antigravity|codex|opencode] [--limit N] [--dry-run]`: Scan finished top-level chat transcripts from Claude Code, Antigravity, Codex, and OpenCode, resolve canonical repository project names and GitHub remotes, redact detected secret matches, diff hashes against `/api/v1/chats/hashes`, and queue new or updated conversations for async GPU distillation via `/api/v1/chats/ingest`.
- `krakozhia sync-github`: Trigger an immediate GitHub roadmap sync pass via `POST /api/v1/github/sync`.
- `krakozhia save --name <name> --type <spec|plan|report|guide|decision> [--project P] [--title T] [--agent A] <file>`: Secret-scan and upload a Markdown document revision. Defaults `--project` to the canonical repository name.
- `krakozhia show [--revision N] [--note] <id>`: Fetch and print a document, document section (`<id>#<index>`), or memory note.
- `krakozhia list [--project P] [--type T] [-k N]`: List stored documents.
- `krakozhia bench record|query|compare ...`: Record structured numeric benchmarks, filter past runs, or compare runs chronologically for one project, suite, and metric.
- `krakozhia brief <project>`: Print the auto-generated Markdown brief for a project.
- `krakozhia eval [--rerank] [--kind note|doc|""] [--max-distance F] [--min-text-score F] <testset.jsonl>`: Measure recall@K, MRR, false-injection rate, p50 latency, and p95 latency across `text`, `vector`, `hybrid`, and optional `hybrid+rerank` forms.

## Claude Code Plugin

Install the plugin from a local clone or marketplace entry so Claude Code runs `krakozhia recall` on `UserPromptSubmit` and `krakozhia sync` asynchronously on `Stop`, and loads the `memory-search` and `save-document` skills.

## MCP Client Configuration

### Claude Code

```sh
claude mcp add --transport http --scope user krakozhia https://kb.lab.rustatian.me/mcp \
  --header "Authorization: Bearer ${KRAKOZHIA_API_TOKEN}"
```

### Antigravity / Gemini

Add to your `mcp_config.json`:

```json
{
  "mcpServers": {
    "krakozhia": {
      "serverUrl": "https://kb.lab.rustatian.me/mcp",
      "headers": {
        "Authorization": "Bearer ${KRAKOZHIA_API_TOKEN}"
      }
    }
  }
}
```

### Codex

Add to `~/.codex/config.toml` and export `KRAKOZHIA_API_TOKEN`:

```toml
[mcp_servers.krakozhia]
url = "https://kb.lab.rustatian.me/mcp"
bearer_token_env_var = "KRAKOZHIA_API_TOKEN"
```

### OpenCode

Add to `opencode.json`:

```json
{
  "mcp": {
    "krakozhia": {
      "type": "remote",
      "url": "https://kb.lab.rustatian.me/mcp",
      "headers": {
        "Authorization": "Bearer {env:KRAKOZHIA_API_TOKEN}"
      }
    }
  }
}
```
