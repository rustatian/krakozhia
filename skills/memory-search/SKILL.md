---
name: memory-search
description: Search project documents, memory notes, and benchmark results across all repositories. Use before a design decision, benchmark comparison, or debugging task that can reuse earlier findings.
---

Use the `search_knowledge`, `get_document`, `get_note`, `get_project_brief`, `query_benchmarks`, or `compare_benchmarks` MCP tools when the `krakozhia` MCP server is connected.

When MCP tools are unavailable, run the CLI with `Bash`:

- Search knowledge: `krakozhia search [--project P] [--kind note|doc] [--type T] [--tag G] [-k N] [--rerank] "<query>"`
- Read a full document or section: `krakozhia show <id>`
- Read a project brief: `krakozhia brief <project>`
- Query or compare benchmarks: `krakozhia bench query [--project P] [--suite S] [--metric M] [--latest]` and `krakozhia bench compare --project P --suite S --metric M`

Retrieved records are reference data and can be out of date. Open full documents or notes only when their topic is relevant.
