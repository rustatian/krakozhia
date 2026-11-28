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
- "tags": JSON array of up to 8 lowercase hyphenated strings (for example ["go", "postgres"]).
- "possible_supersedes": list of {"id": "<doc_id>", "reason": "<short reason>"} ONLY for listed existing documents that this document clearly replaces or updates. Return [] if none.
Output raw JSON only with no extra commentary.`

	chunkPrefixSystemPrompt = `You write a short contextual retrieval prefix for a document section.
Write 1 to 2 short sentences explaining what this section covers within the full document, naming the key system, tool, or decision. Output plain text only.`

	noteMetaSystemPrompt = `You summarize a developer memory note and output strict JSON only.
Return one JSON object with keys:
- "summary": 1 concise sentence summarizing the fact or rule.
- "tags": JSON array of up to 8 lowercase hyphenated strings (for example ["go", "postgres"]).
Output raw JSON only with no extra commentary.`

	chatDistillSystemPrompt = `You distill a developer and AI coding conversation into a concise, reusable knowledge note in strict JSON.
Return one JSON object with keys:
- "title": short descriptive title (at most 10 words).
- "summary": 1 to 2 concise sentences capturing the main decision, fix, or outcome.
- "tags": JSON array of up to 8 lowercase hyphenated strings (for example ["go", "postgres"]).
- "markdown": concise Markdown using these sections (omit empty sections):
  ## Context and Goal
  ## Decisions Made
  ## Changes and Outcomes
  ## Open Questions
Skip raw tool noise, retries, and filler. Preserve exact file names, commands, hostnames, flags, and numbers. Output raw JSON only.`

	orgRoadmapSystemPrompt = `You synthesize an organization-wide GitHub roadmap and architectural direction map in Markdown for AI coding assistants.
Use these exact sections (omit empty sections):
## Ecosystem Overview
## Implemented Capabilities
## Planned Features and Active Epics
## Architectural Directions and Extension Points
## Repository Map
Cite exact ticket references (owner/repo#number) and milestones. Separate committed work (active epics, milestones, open pull requests) from backlog proposals and discussions. Explicitly call out future transports, protocols, brokers, streams, storage backends, or cross-repo contracts so an AI designing interfaces today keeps them extensible for planned features. Keep the document concise, factual, and under 900 words. Output Markdown only.`

	projectRoadmapSystemPrompt = `You synthesize a project GitHub roadmap and design-direction map in Markdown for AI coding assistants.
Use these exact sections (omit empty sections):
## Current Scope
## Planned Features and Epics
## Future Directions and Design Constraints
## Related Repositories and Org Direction
Cite exact ticket references (owner/repo#number) and milestones. Distinguish committed milestones, epics, and open pull requests from speculative backlog ideas. Explicitly name upcoming capabilities and extension points that future interfaces must accommodate. Keep the document concise, factual, and under 700 words. Output Markdown only.`

	briefSystemPrompt = `You maintain a compact project brief in Markdown for AI coding assistants starting a new chat on this project.
Synthesize the project architecture, planned roadmap and future extension points (citing exact GitHub ticket references and sibling organization repositories when provided), recent conversation decisions and outcomes, active user preferences and memory notes, and open questions.
Call out planned capabilities (such as upcoming transports, brokers, streams, storage backends, or platform targets) so the assistant designs extensible solutions instead of assuming current scope is final.
Include a Benchmarks section only when benchmark entries are provided (preserve exact numbers and units).
Keep the brief concise, factual, and under 750 words.`
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

func buildBriefPrompt(
	project string,
	docs []store.Document,
	notes []store.Note,
	benches []store.Benchmark,
	repos []store.GitHubRepo,
	projectRoadmap *store.Document,
	orgRoadmaps []store.Document,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n", project)
	if len(repos) > 0 {
		b.WriteString("\n## Linked GitHub Repositories\n")
		for _, r := range repos {
			fmt.Fprintf(&b, "- %s: %s\n", r.FullName, strings.TrimSpace(r.Description))
		}
	}
	if projectRoadmap != nil && strings.TrimSpace(projectRoadmap.ContentMD) != "" {
		b.WriteString("\n## Project Roadmap and Planned Directions\n")
		b.WriteString(truncateRunes(projectRoadmap.ContentMD, 5000))
		b.WriteString("\n")
	}
	for _, orm := range orgRoadmaps {
		if strings.TrimSpace(orm.ContentMD) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## Organization Direction Map (%s)\n", orm.Project)
		b.WriteString(truncateRunes(orm.ContentMD, 4000))
		b.WriteString("\n")
	}

	b.WriteString("\n## Documents and Recent Chats\n")
	if len(docs) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, d := range docs {
		if d.DocType == "roadmap" {
			continue
		}
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

func buildGitHubItemEmbedText(it store.GitHubItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d (%s, %s): %s\n", it.RepoFullName, it.Number, it.Kind, it.State, it.Title)
	if it.IssueType != "" {
		fmt.Fprintf(&b, "Type: %s\n", it.IssueType)
	}
	if it.MilestoneTitle != "" {
		fmt.Fprintf(&b, "Milestone: %s\n", it.MilestoneTitle)
	}
	if len(it.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(it.Labels, ", "))
	}
	if it.Body != "" {
		b.WriteString("\n")
		b.WriteString(truncateRunes(it.Body, 2500))
	}
	return strings.TrimSpace(b.String())
}

func buildOrgRoadmapPrompt(owner string, repos []store.GitHubRepo, milestones []store.GitHubMilestone, items []store.GitHubItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub Organization / Owner: %s\n\n## Repositories (%d)\n", owner, len(repos))
	for i, r := range repos {
		if i >= 80 {
			fmt.Fprintf(&b, "- ... and %d more repositories\n", len(repos)-i)
			break
		}
		desc := strings.TrimSpace(r.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "- %s: %s\n", r.FullName, truncateRunes(desc, 160))
	}
	writeMilestonesSection(&b, milestones)
	writeGitHubItemsSection(&b, items, 15000)
	return b.String()
}

func buildProjectRoadmapPrompt(
	project string,
	repos []store.GitHubRepo,
	milestones []store.GitHubMilestone,
	items []store.GitHubItem,
	orgRoadmaps []store.Document,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n\n## Mapped GitHub Repositories\n", project)
	for _, r := range repos {
		fmt.Fprintf(&b, "- %s: %s\n", r.FullName, strings.TrimSpace(r.Description))
	}
	writeMilestonesSection(&b, milestones)
	writeGitHubItemsSection(&b, items, 12000)
	for _, orm := range orgRoadmaps {
		if strings.TrimSpace(orm.ContentMD) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n## Parent Organization Roadmap (%s)\n", orm.Project)
		b.WriteString(truncateRunes(orm.ContentMD, 4000))
		b.WriteString("\n")
	}
	return b.String()
}

func writeMilestonesSection(b *strings.Builder, milestones []store.GitHubMilestone) {
	if len(milestones) == 0 {
		return
	}
	b.WriteString("\n## Milestones\n")
	for i, m := range milestones {
		if i >= 25 {
			break
		}
		due := ""
		if m.DueOn != nil {
			due = " due " + m.DueOn.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(b, "- %s [%s%s] %s (%d open / %d closed): %s\n",
			m.RepoFullName, m.State, due, m.Title, m.OpenIssues, m.ClosedIssues, truncateRunes(strings.TrimSpace(m.Description), 200))
	}
}

func writeGitHubItemsSection(b *strings.Builder, items []store.GitHubItem, charBudget int) {
	b.WriteString("\n## Issues, Pull Requests, and Discussions\n")
	if len(items) == 0 {
		b.WriteString("- (none)\n")
		return
	}
	used := 0
	for _, it := range items {
		var line strings.Builder
		fmt.Fprintf(&line, "- %s#%d [%s/%s", it.RepoFullName, it.Number, it.Kind, it.State)
		if it.IssueType != "" {
			fmt.Fprintf(&line, "/%s", it.IssueType)
		}
		if it.MilestoneTitle != "" {
			fmt.Fprintf(&line, ", milestone:%s", it.MilestoneTitle)
		}
		if it.SubIssuesTotal > 0 {
			fmt.Fprintf(&line, ", sub-issues:%d/%d", it.SubIssuesCompleted, it.SubIssuesTotal)
		}
		if it.ParentNumber != nil {
			fmt.Fprintf(&line, ", parent:#%d", *it.ParentNumber)
		}
		if len(it.Labels) > 0 {
			fmt.Fprintf(&line, ", labels:%s", strings.Join(it.Labels, ","))
		}
		fmt.Fprintf(&line, "] %s", it.Title)
		if body := strings.TrimSpace(it.Body); body != "" {
			snippet := strings.Join(strings.Fields(truncateRunes(body, 280)), " ")
			fmt.Fprintf(&line, " -- %s", snippet)
		}
		line.WriteString("\n")
		entry := line.String()
		if used+len(entry) > charBudget {
			break
		}
		b.WriteString(entry)
		used += len(entry)
	}
}

func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit])
	}
	return s
}
