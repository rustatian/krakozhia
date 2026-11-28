package enrich

import (
	"fmt"
	"strings"

	"github.com/rustatian/krakozhia/internal/store"
)

const (
	docMetaSystemPrompt = `You summarize technical knowledge documents and output strict JSON only.
Return one JSON object with keys:
- "summary": 1 to 2 concise sentences stating the purpose and main decision or finding.
- "tags": up to 8 lowercase hyphenated tags (languages, tools, systems, hosts).
- "possible_supersedes": list of {"id": "<doc_id>", "reason": "<short reason>"} ONLY for listed existing documents that this document clearly replaces or updates. Return [] if none.
Output raw JSON only with no extra commentary.`

	chunkPrefixSystemPrompt = `You write a short contextual retrieval prefix for a document section.
Write 1 to 2 short sentences explaining what this section covers within the full document, naming the key system, tool, or decision. Output plain text only.`

	noteMetaSystemPrompt = `You summarize a developer memory note and output strict JSON only.
Return one JSON object with keys:
- "summary": 1 concise sentence summarizing the fact or rule.
- "tags": up to 8 lowercase hyphenated tags.
Output raw JSON only with no extra commentary.`

	chatDistillSystemPrompt = `You distill a developer and AI coding conversation into a concise, reusable knowledge note in strict JSON.
Return one JSON object with keys:
- "title": short descriptive title (at most 10 words).
- "summary": 1 to 2 concise sentences capturing the main decision, fix, or outcome.
- "tags": up to 8 lowercase hyphenated tags (tools, languages, hosts, components).
- "markdown": concise Markdown using these sections (omit empty sections):
  ## Context and Goal
  ## Decisions Made
  ## Changes and Outcomes
  ## Open Questions
Skip raw tool noise, retries, and filler. Preserve exact file names, commands, hostnames, flags, and numbers. Output raw JSON only.`

	briefSystemPrompt = `You maintain a compact project brief in Markdown for AI coding assistants starting a new chat on this project.
Synthesize the project architecture, recent conversation history and outcomes, active decisions, user preferences, and open questions from the provided documents, chat summaries, memory notes, and optional benchmarks.
Include a Benchmarks section only when benchmark entries are provided (preserve exact numbers and units).
Keep the brief concise, factual, and under 500 words.`
)

func buildChatDistillPrompt(cs store.ChatSession) string {
	return fmt.Sprintf("Project: %s\nAgent: %s\nDate: %s\nWorkspace: %s\nOriginal Title: %s\n\nConversation transcript:\n%s",
		cs.Project, cs.SourceAgent, cs.UpdatedAt.UTC().Format("2006-01-02"), cs.WorkspacePath, cs.Title, truncateRunes(cs.RawExcerpt, 20000))
}

func buildDocMetaPrompt(doc store.Document, peers []store.Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Target document ID: %s\nTitle: %s\nType: %s\n\nExisting documents in project %s:\n",
		doc.ID, doc.Title, doc.DocType, doc.Project)
	count := 0
	for _, p := range peers {
		if p.ID == doc.ID {
			continue
		}
		count++
		fmt.Fprintf(&b, "- %s (%s): %s\n", p.ID, p.DocType, p.Title)
	}
	if count == 0 {
		b.WriteString("- (none)\n")
	}
	b.WriteString("\nDocument content:\n")
	b.WriteString(truncateRunes(doc.ContentMD, 12000))
	return b.String()
}

func buildChunkPrefixPrompt(doc store.Document, ch store.Chunk) string {
	return fmt.Sprintf("Document ID: %s\nTitle: %s\nSection: %s\n\nDocument excerpt:\n%s\n\nSection text:\n%s",
		doc.ID, doc.Title, ch.HeadingPath, truncateRunes(doc.ContentMD, 4000), truncateRunes(ch.Content, 4000))
}

func buildNoteMetaPrompt(n store.Note) string {
	return fmt.Sprintf("Note ID: %s\nName: %s\nType: %s\nDescription: %s\n\nContent:\n%s",
		n.ID, n.Name, n.NoteType, n.Description, truncateRunes(n.Content, 6000))
}

func buildBriefPrompt(project string, docs []store.Document, notes []store.Note, benches []store.Benchmark) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\n## Documents\n", project)
	if len(docs) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, d := range docs {
		text := d.Summary
		if text == "" {
			text = d.Title
		}
		fmt.Fprintf(&b, "- %s (%s, rev %d): %s - %s\n", d.ID, d.DocType, d.CurrentRevision, d.Title, text)
	}
	b.WriteString("\n## Memory Notes\n")
	if len(notes) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, n := range notes {
		text := n.Summary
		if text == "" {
			text = n.Description
		}
		fmt.Fprintf(&b, "- %s (%s): %s - %s\n", n.ID, n.NoteType, n.Name, text)
	}
	if len(benches) > 0 {
		b.WriteString("\n## Latest Benchmarks\n")
		for _, bm := range benches {
			delta := ""
			if bm.DeltaPct != nil {
				delta = fmt.Sprintf(" (delta %.2f%%)", *bm.DeltaPct)
			}
			fmt.Fprintf(&b, "- %s/%s: %g %s [%s]%s\n", bm.Suite, bm.Metric, bm.Value, bm.Unit, bm.Status, delta)
		}
	}
	return b.String()
}

func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit])
	}
	return s
}
