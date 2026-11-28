// Package enrich asynchronously generates derived summaries, chunk context prefixes, tags, links, and project briefs.
package enrich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/chunk"
	"github.com/rustatian/krakozhia/internal/secrets"
	"github.com/rustatian/krakozhia/internal/store"
)

// InferenceClient abstracts LLM chat and document embedding operations.
type InferenceClient interface {
	EmbedDocument(ctx context.Context, text string) ([]float32, error)
	Chat(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error)
}

// Config configures an enrichment Worker.
type Config struct {
	Store           *store.Store
	Inference       InferenceClient
	PollInterval    time.Duration
	MaxChunksPerDoc int
	Logger          *slog.Logger
}

// Worker processes queued enrichment jobs against a local GPU inference server.
type Worker struct {
	store           *store.Store
	inf             InferenceClient
	pollInterval    time.Duration
	maxChunksPerDoc int
	log             *slog.Logger
}

// DocMeta holds parsed document enrichment JSON.
type DocMeta struct {
	Summary            string          `json:"summary"`
	Tags               []string        `json:"tags"`
	PossibleSupersedes []SupersedeHint `json:"possible_supersedes"`
}

// SupersedeHint links a new document to an older candidate document.
type SupersedeHint struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// NoteMeta holds parsed memory note enrichment JSON.
type NoteMeta struct {
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
}

// ChatDistill holds a distilled chat session summary ready to store as a document.
type ChatDistill struct {
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags"`
	Markdown string   `json:"markdown"`
}

// New returns a configured enrichment Worker.
func New(cfg Config) *Worker {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 3 * time.Second
	}
	maxChunks := cfg.MaxChunksPerDoc
	if maxChunks <= 0 {
		maxChunks = 25
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store:           cfg.Store,
		inf:             cfg.Inference,
		pollInterval:    poll,
		maxChunksPerDoc: maxChunks,
		log:             logger,
	}
}

// Run polls and processes enrichment jobs until ctx is canceled.
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		worked, err := w.ProcessOnce(ctx)
		if err != nil {
			w.log.Warn("enrichment job failed", "err", err)
		}
		if worked && ctx.Err() == nil {
			continue
		}
		timer := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ProcessOnce claims and runs one pending job. It returns false when no ready job exists.
func (w *Worker) ProcessOnce(ctx context.Context) (bool, error) {
	job, err := w.store.ClaimNextJob(ctx)
	if err != nil || job == nil {
		return false, err
	}

	var runErr error
	switch job.JobType {
	case "enrich_doc":
		runErr = w.enrichDoc(ctx, job.TargetID)
	case "enrich_note":
		runErr = w.enrichNote(ctx, job.TargetID)
	case "distill_chat":
		runErr = w.distillChat(ctx, job.TargetID)
	case "embed_github_item":
		runErr = w.embedGitHubItem(ctx, job.TargetID)
	case "distill_org_roadmap":
		runErr = w.distillOrgRoadmap(ctx, job.TargetID)
	case "distill_project_roadmap":
		runErr = w.distillProjectRoadmap(ctx, job.TargetID)
	case "refresh_brief":
		runErr = w.refreshBrief(ctx, job.TargetID)
	default:
		runErr = fmt.Errorf("unknown job_type %q", job.JobType)
	}

	if runErr != nil {
		finCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if fErr := w.store.FailJob(finCtx, job.ID, runErr.Error(), Backoff(job.Attempts)); fErr != nil {
			w.log.Error("failed to record job error", "job_id", job.ID, "err", fErr)
		}
		return true, runErr
	}
	finCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.store.CompleteJob(finCtx, job.ID); err != nil {
		return true, err
	}
	return true, nil
}

func (w *Worker) enrichDoc(ctx context.Context, docID string) error {
	doc, err := w.store.GetDocument(ctx, docID, 0)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	peers, err := w.store.ListDocuments(ctx, doc.Project, "", 12)
	if err != nil {
		return err
	}
	validIDs := make(map[string]bool, len(peers))
	for _, p := range peers {
		validIDs[p.ID] = true
	}

	rawMeta, err := w.inf.Chat(ctx, docMetaSystemPrompt, buildDocMetaPrompt(doc, peers), 512)
	if err != nil {
		return err
	}
	meta, err := ParseDocMeta(rawMeta, validIDs, doc.ID)
	if err != nil {
		return err
	}
	if err := w.store.UpdateDocumentEnrichment(ctx, doc.ID, meta.Summary, meta.Tags); err != nil {
		return err
	}
	for _, link := range meta.PossibleSupersedes {
		if err := w.store.AddRecordLink(ctx, store.RecordLink{
			SourceKind: "doc",
			SourceID:   doc.ID,
			TargetKind: "doc",
			TargetID:   link.ID,
			Relation:   "possible_supersedes",
			Note:       link.Reason,
		}); err != nil {
			return err
		}
	}

	chunks, err := w.store.ListChunks(ctx, doc.ID)
	if err != nil {
		return err
	}
	limit := min(len(chunks), w.maxChunksPerDoc)
	for _, ch := range chunks[:limit] {
		prefix, err := w.inf.Chat(ctx, chunkPrefixSystemPrompt, buildChunkPrefixPrompt(doc, ch), 128)
		if err != nil {
			return err
		}
		prefix = strings.TrimSpace(prefix)
		embedInput := prefix + "\n" + ch.HeadingPath + "\n\n" + ch.Content
		vec, err := w.inf.EmbedDocument(ctx, embedInput)
		if err != nil {
			return err
		}
		if err := w.store.UpdateChunkEnrichment(ctx, ch.ID, prefix, vec); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) enrichNote(ctx context.Context, noteID string) error {
	n, err := w.store.GetNote(ctx, noteID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rawMeta, err := w.inf.Chat(ctx, noteMetaSystemPrompt, buildNoteMetaPrompt(n), 256)
	if err != nil {
		return err
	}
	meta, err := ParseNoteMeta(rawMeta)
	if err != nil {
		return err
	}
	if err := w.store.UpdateNoteEnrichment(ctx, n.ID, meta.Summary, meta.Tags); err != nil {
		return err
	}
	vec, err := w.inf.EmbedDocument(ctx, n.Content)
	if err != nil {
		return err
	}
	return w.store.UpdateNoteEmbedding(ctx, n.ID, vec)
}

func (w *Worker) distillChat(ctx context.Context, chatID string) error {
	cs, err := w.store.GetChatSession(ctx, chatID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cs.DistilledHash == cs.TranscriptHash || strings.TrimSpace(cs.RawExcerpt) == "" {
		return nil
	}

	raw, err := w.inf.Chat(ctx, chatDistillSystemPrompt, buildChatDistillPrompt(cs), 1024)
	if err != nil {
		return err
	}
	distilled, err := ParseChatDistill(raw, cs.Title)
	if err != nil {
		return err
	}

	var docBody strings.Builder
	fmt.Fprintf(&docBody, "# %s\n\n", distilled.Title)
	fmt.Fprintf(&docBody, "- Agent: `%s`\n- Date: `%s`\n- Session: `%s`\n", cs.SourceAgent, cs.UpdatedAt.UTC().Format(time.DateOnly), cs.ExternalID)
	if cs.WorkspacePath != "" {
		fmt.Fprintf(&docBody, "- Workspace: `%s`\n", cs.WorkspacePath)
	}
	docBody.WriteString("\n")
	docBody.WriteString(strings.TrimSpace(distilled.Markdown))
	docBody.WriteString("\n")

	content, _ := secrets.Redact(docBody.String())
	sections := chunk.Split(content, distilled.Title)
	chunkInputs := make([]store.ChunkInput, len(sections))
	for i, sec := range sections {
		chunkInputs[i] = store.ChunkInput{Section: sec}
		if vec, err := w.inf.EmbedDocument(ctx, sec.EmbedText()); err == nil {
			chunkInputs[i].Embedding = vec
		}
	}

	docName := chatDocumentName(cs.SourceAgent, cs.ExternalID)
	saved, err := w.store.SaveDocument(ctx, store.SaveDocumentInput{
		Project:       cs.Project,
		Name:          docName,
		DocType:       "chat",
		Title:         distilled.Title,
		Summary:       distilled.Summary,
		Tags:          distilled.Tags,
		Content:       content,
		SourceAgent:   cs.SourceAgent,
		SkipEnrichDoc: true,
		Chunks:        chunkInputs,
	})
	if err != nil {
		return err
	}
	return w.store.MarkChatDistilled(ctx, cs.ID, saved.ID, cs.TranscriptHash)
}

func (w *Worker) embedGitHubItem(ctx context.Context, itemID string) error {
	item, err := w.store.GetGitHubItem(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	vec, err := w.inf.EmbedDocument(ctx, buildGitHubItemEmbedText(item))
	if err != nil {
		return err
	}
	return w.store.UpdateGitHubItemEmbedding(ctx, item.ID, vec)
}

func (w *Worker) distillOrgRoadmap(ctx context.Context, owner string) error {
	repos, err := w.store.ListGitHubReposByOwner(ctx, owner)
	if err != nil {
		return err
	}
	items, err := w.store.ListGitHubItems(ctx, store.GitHubItemFilter{Owner: owner, Limit: 160})
	if err != nil {
		return err
	}
	if len(repos) == 0 && len(items) == 0 {
		return nil
	}
	var milestones []store.GitHubMilestone
	for i, r := range repos {
		if i >= 30 {
			break
		}
		ms, err := w.store.ListGitHubMilestones(ctx, r.FullName)
		if err != nil {
			return err
		}
		milestones = append(milestones, ms...)
	}

	rawMD, err := w.inf.Chat(ctx, orgRoadmapSystemPrompt, buildOrgRoadmapPrompt(owner, repos, milestones, items), 1400)
	if err != nil {
		return err
	}
	body := stripCodeFence(rawMD)
	if body == "" {
		return errors.New("empty org roadmap response")
	}

	title := fmt.Sprintf("%s Organization Roadmap", owner)
	var docBody strings.Builder
	fmt.Fprintf(&docBody, "# %s\n\n", title)
	fmt.Fprintf(&docBody, "- Owner: `%s`\n- Repositories tracked: `%d`\n- Items synthesized: `%d`\n\n", owner, len(repos), len(items))
	docBody.WriteString(body)
	docBody.WriteString("\n")

	content, _ := secrets.Redact(docBody.String())
	if err := w.saveRoadmapDocument(ctx, owner, "org-roadmap", title, firstSummaryLine(body, title), []string{"roadmap", "github", strings.ToLower(owner)}, content, true); err != nil {
		return err
	}

	projects, err := w.store.ListProjectsForOwner(ctx, owner)
	if err != nil {
		return err
	}
	for _, proj := range projects {
		if err := w.store.EnqueueJob(ctx, "refresh_brief", "project", proj); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) distillProjectRoadmap(ctx context.Context, project string) error {
	repos, err := w.store.ListReposForProject(ctx, project)
	if err != nil {
		return err
	}
	items, err := w.store.ListGitHubItems(ctx, store.GitHubItemFilter{Project: project, Limit: 120})
	if err != nil {
		return err
	}
	if len(repos) == 0 && len(items) == 0 {
		return nil
	}
	var milestones []store.GitHubMilestone
	for _, r := range repos {
		ms, err := w.store.ListGitHubMilestones(ctx, r.FullName)
		if err != nil {
			return err
		}
		milestones = append(milestones, ms...)
	}
	orgRoadmaps, err := w.loadOrgRoadmapsForProject(ctx, project)
	if err != nil {
		return err
	}

	rawMD, err := w.inf.Chat(ctx, projectRoadmapSystemPrompt, buildProjectRoadmapPrompt(project, repos, milestones, items, orgRoadmaps), 1200)
	if err != nil {
		return err
	}
	body := stripCodeFence(rawMD)
	if body == "" {
		return errors.New("empty project roadmap response")
	}

	title := fmt.Sprintf("%s Roadmap", project)
	var docBody strings.Builder
	fmt.Fprintf(&docBody, "# %s\n\n", title)
	if len(repos) > 0 {
		names := make([]string, len(repos))
		for i, r := range repos {
			names[i] = r.FullName
		}
		fmt.Fprintf(&docBody, "- Repositories: `%s`\n", strings.Join(names, "`, `"))
	}
	fmt.Fprintf(&docBody, "- Items synthesized: `%d`\n\n", len(items))
	docBody.WriteString(body)
	docBody.WriteString("\n")

	content, _ := secrets.Redact(docBody.String())
	tags := []string{"roadmap", "github"}
	for _, orm := range orgRoadmaps {
		tags = append(tags, strings.ToLower(orm.Project))
	}
	return w.saveRoadmapDocument(ctx, project, "roadmap", title, firstSummaryLine(body, title), normalizeTags(tags), content, false)
}

func (w *Worker) saveRoadmapDocument(ctx context.Context, project, name, title, summary string, tags []string, content string, skipRefreshBrief bool) error {
	sections := chunk.Split(content, title)
	chunkInputs := make([]store.ChunkInput, len(sections))
	for i, sec := range sections {
		chunkInputs[i] = store.ChunkInput{Section: sec}
		if vec, err := w.inf.EmbedDocument(ctx, sec.EmbedText()); err == nil {
			chunkInputs[i].Embedding = vec
		}
	}
	_, err := w.store.SaveDocument(ctx, store.SaveDocumentInput{
		Project:          project,
		Name:             name,
		DocType:          "roadmap",
		Title:            title,
		Summary:          summary,
		Tags:             tags,
		Content:          content,
		SourceAgent:      "githubsync",
		SkipEnrichDoc:    true,
		SkipRefreshBrief: skipRefreshBrief,
		Chunks:           chunkInputs,
	})
	return err
}

func (w *Worker) loadOrgRoadmapsForProject(ctx context.Context, project string) ([]store.Document, error) {
	owners, err := w.store.ListOwnersForProject(ctx, project)
	if err != nil {
		return nil, err
	}
	var out []store.Document
	for _, owner := range owners {
		doc, err := w.store.GetDocument(ctx, owner+"/org-roadmap", 0)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}

func firstSummaryLine(md, fallback string) string {
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimLeft(trimmed, "-* ")
		if trimmed != "" {
			return truncateRunes(trimmed, 220)
		}
	}
	return fallback
}

func (w *Worker) refreshBrief(ctx context.Context, project string) error {
	docs, err := w.store.ListDocuments(ctx, project, "", 25)
	if err != nil {
		return err
	}
	notes, err := w.store.ListNotes(ctx, project, "", 20)
	if err != nil {
		return err
	}
	benches, err := w.store.QueryBenchmarks(ctx, store.BenchmarkFilter{
		Project:    project,
		LatestOnly: true,
		Limit:      30,
	})
	if err != nil {
		return err
	}
	repos, err := w.store.ListReposForProject(ctx, project)
	if err != nil {
		return err
	}
	var projectRoadmap *store.Document
	if doc, err := w.store.GetDocument(ctx, project+"/roadmap", 0); err == nil {
		projectRoadmap = &doc
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	orgRoadmaps, err := w.loadOrgRoadmapsForProject(ctx, project)
	if err != nil {
		return err
	}
	if len(docs) == 0 && len(notes) == 0 && len(benches) == 0 && projectRoadmap == nil && len(orgRoadmaps) == 0 {
		return nil
	}

	docIDs := make([]string, 0, len(docs))
	for _, d := range docs {
		docIDs = append(docIDs, d.ID)
	}
	superseded, err := w.store.ListSupersededDocs(ctx, docIDs)
	if err != nil {
		return err
	}

	sourceHash := hashBriefInputs(docs, notes, benches, projectRoadmap, orgRoadmaps)
	existing, err := w.store.GetProjectBrief(ctx, project)
	if err == nil && existing.SourceHash == sourceHash {
		return nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	briefMD, err := w.inf.Chat(ctx, briefSystemPrompt, buildBriefPrompt(project, time.Now().UTC(), docs, superseded, notes, benches, repos, projectRoadmap, orgRoadmaps), 1280)
	if err != nil {
		return err
	}
	briefMD = stripCodeFence(briefMD)
	if briefMD == "" {
		return errors.New("empty project brief response")
	}
	return w.store.UpsertProjectBrief(ctx, project, briefMD, sourceHash)
}

// ParseDocMeta parses and validates JSON document enrichment output.
func ParseDocMeta(raw string, validTargetIDs map[string]bool, selfID string) (DocMeta, error) {
	cleaned := extractJSONObject(raw)
	var wire struct {
		Summary            string          `json:"summary"`
		Tags               json.RawMessage `json:"tags"`
		PossibleSupersedes []SupersedeHint `json:"possible_supersedes"`
	}
	if err := json.Unmarshal([]byte(cleaned), &wire); err != nil {
		return DocMeta{}, fmt.Errorf("parse doc meta: %w", err)
	}
	meta := DocMeta{
		Summary: strings.TrimSpace(wire.Summary),
		Tags:    parseFlexibleTags(wire.Tags),
	}
	if meta.Summary == "" {
		return DocMeta{}, errors.New("doc summary is empty")
	}

	var filtered []SupersedeHint
	for _, s := range wire.PossibleSupersedes {
		id := strings.TrimSpace(s.ID)
		if id == "" || id == selfID || !validTargetIDs[id] {
			continue
		}
		filtered = append(filtered, SupersedeHint{ID: id, Reason: strings.TrimSpace(s.Reason)})
	}
	meta.PossibleSupersedes = filtered
	return meta, nil
}

// ParseNoteMeta parses and validates JSON note enrichment output.
func ParseNoteMeta(raw string) (NoteMeta, error) {
	cleaned := extractJSONObject(raw)
	var wire struct {
		Summary string          `json:"summary"`
		Tags    json.RawMessage `json:"tags"`
	}
	if err := json.Unmarshal([]byte(cleaned), &wire); err != nil {
		return NoteMeta{}, fmt.Errorf("parse note meta: %w", err)
	}
	meta := NoteMeta{
		Summary: strings.TrimSpace(wire.Summary),
		Tags:    parseFlexibleTags(wire.Tags),
	}
	if meta.Summary == "" {
		return NoteMeta{}, errors.New("note summary is empty")
	}
	return meta, nil
}

// ParseChatDistill parses structured JSON chat distillation output.
func ParseChatDistill(raw, fallbackTitle string) (ChatDistill, error) {
	cleaned := extractJSONObject(raw)
	if cleaned == "" {
		return ChatDistill{}, errors.New("empty chat distillation response")
	}
	var wire struct {
		Title    string          `json:"title"`
		Summary  string          `json:"summary"`
		Tags     json.RawMessage `json:"tags"`
		Markdown string          `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(cleaned), &wire); err != nil {
		return ChatDistill{}, fmt.Errorf("parse chat distill json: %w", err)
	}
	md := strings.TrimSpace(wire.Markdown)
	if md == "" {
		return ChatDistill{}, errors.New("chat distillation markdown is empty")
	}
	title := strings.TrimSpace(wire.Title)
	if title == "" {
		title = strings.TrimSpace(fallbackTitle)
	}
	if title == "" {
		title = "Session Summary"
	}
	summary := strings.TrimSpace(wire.Summary)
	if summary == "" {
		summary = title
	}
	tags := parseFlexibleTags(wire.Tags)
	if len(tags) == 0 {
		tags = []string{"chat"}
	}
	return ChatDistill{
		Title:    title,
		Summary:  summary,
		Tags:     tags,
		Markdown: md,
	}, nil
}

func parseFlexibleTags(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return normalizeTags(list)
	}
	var joined string
	if err := json.Unmarshal(raw, &joined); err != nil {
		return nil
	}
	replacer := strings.NewReplacer(",", " ", ";", " ", "|", " ")
	fields := strings.Fields(replacer.Replace(joined))
	var out []string
	for _, f := range fields {
		if trimmed := strings.Trim(f, "-"); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return normalizeTags(out)
}

func stripCodeFence(raw string) string {
	s := stripThoughtBlocks(raw)
	if strings.HasPrefix(s, "```") {
		if _, after, ok := strings.Cut(s, "\n"); ok {
			s = after
		} else {
			s = strings.TrimPrefix(s, "```")
		}
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func extractJSONObject(raw string) string {
	s := stripCodeFence(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return strings.TrimSpace(s[start : end+1])
	}
	return s
}

func stripThoughtBlocks(raw string) string {
	s := strings.TrimSpace(raw)
	for _, pair := range [][2]string{
		{"<think>", "</think>"},
		{"<|channel>thought", "<channel|>"},
	} {
		for {
			start := strings.Index(s, pair[0])
			if start < 0 {
				break
			}
			end := strings.Index(s[start+len(pair[0]):], pair[1])
			if end < 0 {
				break
			}
			s = strings.TrimSpace(s[:start] + s[start+len(pair[0])+end+len(pair[1]):])
		}
	}
	return s
}

func chatDocumentName(agent, externalID string) string {
	clean := func(s string, max int) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			} else if r == '_' || r == '.' || r == ':' || r == '/' {
				b.WriteByte('-')
			}
			if b.Len() >= max {
				break
			}
		}
		return strings.Trim(b.String(), "-")
	}
	a := clean(agent, 12)
	if a == "" {
		a = "agent"
	}
	id := clean(externalID, 32)
	if id == "" {
		id = "session"
	}
	return "chat-" + a + "-" + id
}

// Backoff returns exponential retry delay starting at 30s and capped at 15m.
func Backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 30 * time.Second
	}
	delay := 30 * time.Second << min(attempt-1, 10)
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func normalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		norm := strings.ToLower(strings.TrimSpace(t))
		norm = strings.ReplaceAll(norm, " ", "-")
		if norm == "" || slices.Contains(out, norm) {
			continue
		}
		out = append(out, norm)
		if len(out) == 8 {
			break
		}
	}
	return out
}

func hashBriefInputs(
	docs []store.Document,
	notes []store.Note,
	benches []store.Benchmark,
	projectRoadmap *store.Document,
	orgRoadmaps []store.Document,
) string {
	h := sha256.New()
	if projectRoadmap != nil {
		fmt.Fprintf(h, "prm:%s:%d:%s\n", projectRoadmap.ID, projectRoadmap.CurrentRevision, projectRoadmap.Summary)
	}
	for _, orm := range orgRoadmaps {
		fmt.Fprintf(h, "orm:%s:%d:%s\n", orm.ID, orm.CurrentRevision, orm.Summary)
	}
	for _, d := range docs {
		fmt.Fprintf(h, "d:%s:%d:%s\n", d.ID, d.CurrentRevision, d.Summary)
	}
	for _, n := range notes {
		fmt.Fprintf(h, "n:%s:%s:%s\n", n.ID, n.ContentHash, n.Summary)
	}
	for _, b := range benches {
		fmt.Fprintf(h, "b:%d:%s:%s:%g:%s\n", b.ID, b.Suite, b.Metric, b.Value, b.Status)
	}
	return hex.EncodeToString(h.Sum(nil))
}
