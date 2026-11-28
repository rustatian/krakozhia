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
	case "refresh_brief":
		runErr = w.refreshBrief(ctx, job.TargetID)
	default:
		runErr = fmt.Errorf("unknown job_type %q", job.JobType)
	}

	if runErr != nil {
		if fErr := w.store.FailJob(ctx, job.ID, runErr.Error(), Backoff(job.Attempts)); fErr != nil {
			w.log.Error("failed to record job error", "job_id", job.ID, "err", fErr)
		}
		return true, runErr
	}
	if err := w.store.CompleteJob(ctx, job.ID); err != nil {
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

func (w *Worker) refreshBrief(ctx context.Context, project string) error {
	docs, err := w.store.ListDocuments(ctx, project, "", 25)
	if err != nil {
		return err
	}
	notes, err := w.store.ListNotesForBrief(ctx, project, 20)
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
	if len(docs) == 0 && len(notes) == 0 && len(benches) == 0 {
		return nil
	}

	sourceHash := hashBriefInputs(docs, notes, benches)
	existing, err := w.store.GetProjectBrief(ctx, project)
	if err == nil && existing.SourceHash == sourceHash {
		return nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	briefMD, err := w.inf.Chat(ctx, briefSystemPrompt, buildBriefPrompt(project, docs, notes, benches), 768)
	if err != nil {
		return err
	}
	briefMD = strings.TrimSpace(briefMD)
	if briefMD == "" {
		return errors.New("empty project brief response")
	}
	return w.store.UpsertProjectBrief(ctx, project, briefMD, sourceHash)
}

// ParseDocMeta parses and validates JSON document enrichment output.
func ParseDocMeta(raw string, validTargetIDs map[string]bool, selfID string) (DocMeta, error) {
	cleaned := stripJSONFence(raw)
	var meta DocMeta
	if err := json.Unmarshal([]byte(cleaned), &meta); err != nil {
		return DocMeta{}, fmt.Errorf("parse doc meta: %w", err)
	}
	meta.Summary = strings.TrimSpace(meta.Summary)
	if meta.Summary == "" {
		return DocMeta{}, errors.New("doc summary is empty")
	}
	meta.Tags = normalizeTags(meta.Tags)

	var filtered []SupersedeHint
	for _, s := range meta.PossibleSupersedes {
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
	cleaned := stripJSONFence(raw)
	var meta NoteMeta
	if err := json.Unmarshal([]byte(cleaned), &meta); err != nil {
		return NoteMeta{}, fmt.Errorf("parse note meta: %w", err)
	}
	meta.Summary = strings.TrimSpace(meta.Summary)
	if meta.Summary == "" {
		return NoteMeta{}, errors.New("note summary is empty")
	}
	meta.Tags = normalizeTags(meta.Tags)
	return meta, nil
}

// ParseChatDistill parses structured JSON chat distillation output or falls back to plain Markdown.
func ParseChatDistill(raw, fallbackTitle string) (ChatDistill, error) {
	cleaned := stripJSONFence(raw)
	if cleaned == "" {
		return ChatDistill{}, errors.New("empty chat distillation response")
	}
	var d ChatDistill
	if err := json.Unmarshal([]byte(cleaned), &d); err == nil && strings.TrimSpace(d.Markdown) != "" {
		d.Title = strings.TrimSpace(d.Title)
		if d.Title == "" {
			d.Title = strings.TrimSpace(fallbackTitle)
		}
		if d.Title == "" {
			d.Title = "Session Summary"
		}
		d.Summary = strings.TrimSpace(d.Summary)
		if d.Summary == "" {
			d.Summary = d.Title
		}
		d.Tags = normalizeTags(d.Tags)
		if len(d.Tags) == 0 {
			d.Tags = []string{"chat"}
		}
		d.Markdown = strings.TrimSpace(d.Markdown)
		return d, nil
	}

	// Accept raw Markdown when the model skips the JSON wrapper.
	md := stripMarkdownFence(raw)
	if md == "" {
		return ChatDistill{}, errors.New("empty chat distillation response")
	}
	title := chunk.Title(md)
	if title == "" {
		title = strings.TrimSpace(fallbackTitle)
	}
	if title == "" {
		title = "Session Summary"
	}
	return ChatDistill{
		Title:    title,
		Summary:  title,
		Tags:     []string{"chat"},
		Markdown: md,
	}, nil
}

func stripMarkdownFence(raw string) string {
	s := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(s, "```markdown"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "```md"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "```"); ok {
		s = rest
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
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

func stripJSONFence(raw string) string {
	s := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(s, "```json"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "```"); ok {
		s = rest
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
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

func hashBriefInputs(docs []store.Document, notes []store.Note, benches []store.Benchmark) string {
	h := sha256.New()
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
