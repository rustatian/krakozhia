---
name: save-document
description: Save a completed design spec, plan, review report, guide, or benchmark measurement to the central knowledge base. Use instead of committing working design docs into project repositories.
---

Write long-form documents as Markdown in the session scratch folder first, then upload them to the knowledge base.

When the `krakozhia` MCP server is connected, prefer `save_document` and `record_benchmark`.

From `Bash`:

- Save or update a document revision: `krakozhia save --name <name> --type <spec|plan|report|guide|decision> [--project P] <file>`
- Update an existing document: run `krakozhia show <project>/<name>` to read the current revision, write edits to a scratch file, and run `krakozhia save` again with the same `--name`.
- Record a numeric benchmark or improvement measurement: `krakozhia bench record --project P --suite S --metric M --value <num> --unit <unit> [--baseline <id>] [--status baseline|kept|reverted|info] [--commit <sha>] [--branch <name>] [--notes "<summary>"]`
