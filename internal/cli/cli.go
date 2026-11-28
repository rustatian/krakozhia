package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/chats"
	"github.com/rustatian/krakozhia/internal/note"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

const (
	syncBatchSize     = 25
	sessionRetention  = 7 * 24 * time.Hour
	searchSnippetRune = 500
)

type localManifest struct {
	Hashes map[string]string `json:"hashes"`
}

// Run dispatches a CLI command and never exits with code 2.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: krakozhia <serve|recall|search|sync|sync-chats|sync-github|save|show|list|bench|brief|eval>")
	}
	switch args[0] {
	case "recall":
		return runRecall(stdin, stdout)
	case "search":
		return runSearch(args[1:], stdout)
	case "sync":
		return runSync(args[1:], stdout, stderr)
	case "sync-chats":
		return runSyncChats(args[1:], stdout, stderr)
	case "sync-github":
		return runSyncGitHub(stdout, stderr)
	case "save":
		return runSave(args[1:], stdout)
	case "show":
		return runShow(args[1:], stdout)
	case "list":
		return runList(args[1:], stdout)
	case "bench":
		return runBench(args[1:], stdout)
	case "brief":
		return runBrief(args[1:], stdout)
	case "eval":
		return runEval(args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runSearch(args []string, stdout io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "filter to project")
	kind := fs.String("kind", "", "filter by record kind: note, doc, or github")
	typ := fs.String("type", "", "filter by document or note type")
	tag := fs.String("tag", "", "filter by derived tag")
	limit := fs.Int("k", 5, "maximum hits")
	rerank := fs.Bool("rerank", false, "rerank hits with cross-encoder")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("usage: krakozhia search [--project P] [--kind note|doc|github] [--type T] [--tag G] [-k N] [--rerank] <query>")
	}

	projectsDir, err := ProjectsDir()
	if err != nil {
		return err
	}
	client := NewClient(cfg.URL, cfg.Token)
	resp, err := client.Search(context.Background(), server.SearchRequest{
		Form:       store.FormHybrid,
		Query:      truncateRunes(query, maxQueryRunes),
		Kind:       *kind,
		Project:    *project,
		Type:       *typ,
		Tag:        *tag,
		Limit:      *limit,
		DedupeDocs: true,
		Rerank:     *rerank,
	})
	if err != nil {
		return err
	}
	if len(resp.Hits) == 0 {
		fmt.Fprintln(stdout, "No matching records.")
		return nil
	}
	for _, h := range resp.Hits {
		fmt.Fprintln(stdout, formatPointer(h, projectsDir))
		if snip := cleanText(truncateRunes(h.Snippet, searchSnippetRune)); snip != "" {
			fmt.Fprintf(stdout, "  %s\n", snip)
		}
	}
	return nil
}

func runSync(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	stateDir, err := StateDir()
	if err != nil {
		return err
	}
	projectsDir, err := ProjectsDir()
	if err != nil {
		return err
	}
	pruneOldSessions(filepath.Join(stateDir, "sessions"), sessionRetention)

	notes, warnings, err := note.ScanDir(projectsDir)
	if err != nil {
		return err
	}
	homeDir, _ := os.UserHomeDir()
	resolvedSlugs := map[string]string{}
	for i := range notes {
		slug := notes[i].Project
		if canonical, ok := resolvedSlugs[slug]; ok {
			notes[i].Project = canonical
			continue
		}
		canonical := chats.ResolveClaudeFolderProject(projectsDir, slug, homeDir)
		resolvedSlugs[slug] = canonical
		notes[i].Project = canonical
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "skip:", w)
	}

	ctx := context.Background()
	client := NewClient(cfg.URL, cfg.Token)
	serverHashes, err := client.ListNoteHashes(ctx)
	if err != nil {
		return err
	}
	prevManifest := loadManifest(filepath.Join(stateDir, "manifest.json"))

	localByID := make(map[string]note.Note, len(notes))
	var toUpsert []server.SaveNoteRequest
	for _, n := range notes {
		localByID[n.ID] = n
		if serverHashes[n.ID] == n.ContentHash {
			continue
		}
		ts := n.UpdatedAt
		toUpsert = append(toUpsert, server.SaveNoteRequest{
			ID:          n.ID,
			Project:     n.Project,
			NoteType:    n.NoteType,
			Name:        n.Name,
			Description: n.Description,
			Content:     n.Content,
			SourceAgent: cfg.SourceAgent,
			UpdatedAt:   &ts,
		})
	}

	var toDelete []string
	for id := range prevManifest.Hashes {
		if _, onDisk := localByID[id]; !onDisk {
			if _, onServer := serverHashes[id]; onServer {
				toDelete = append(toDelete, id)
			}
		}
	}

	if len(toUpsert) == 0 && len(toDelete) == 0 {
		fmt.Fprintf(stdout, "sync: up to date (%d notes)\n", len(notes))
		return saveManifest(filepath.Join(stateDir, "manifest.json"), localByID, nil)
	}

	totalWritten := 0
	totalDeleted := 0
	failedIDs := map[string]bool{}
	for i := 0; i < len(toUpsert); i += syncBatchSize {
		end := min(i+syncBatchSize, len(toUpsert))
		req := server.SyncNotesRequest{Notes: toUpsert[i:end]}
		if end == len(toUpsert) && len(toDelete) > 0 {
			req.DeleteIDs = toDelete
			toDelete = nil
		}
		res, err := client.SyncNotes(ctx, req)
		if err != nil {
			return err
		}
		totalWritten += res.Written
		totalDeleted += res.Deleted
		for _, e := range res.Errors {
			fmt.Fprintln(stderr, "sync error:", e)
			if id, _, ok := strings.Cut(e, ":"); ok {
				failedIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	if len(toDelete) > 0 {
		res, err := client.SyncNotes(ctx, server.SyncNotesRequest{DeleteIDs: toDelete})
		if err != nil {
			return err
		}
		totalDeleted += res.Deleted
		for _, e := range res.Errors {
			fmt.Fprintln(stderr, "sync error:", e)
		}
	}

	if err := saveManifest(filepath.Join(stateDir, "manifest.json"), localByID, failedIDs); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sync: written=%d deleted=%d skipped=%d total=%d\n",
		totalWritten, totalDeleted, len(warnings)+len(failedIDs), len(notes))
	return nil
}

func runSyncGitHub(stdout, stderr io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	client := NewClient(cfg.URL, cfg.Token)
	client.http.Timeout = 10 * time.Minute
	sum, err := client.SyncGitHub(context.Background())
	if err != nil {
		return err
	}
	for _, e := range sum.Errors {
		fmt.Fprintln(stderr, "sync-github error:", e)
	}
	fmt.Fprintf(stdout, "sync-github: owners=%d repos=%d items=%d changed=%d errors=%d\n",
		sum.Owners, sum.ReposScanned, sum.ItemsUpserted, sum.ItemsChanged, len(sum.Errors))
	return nil
}

func runSave(args []string, stdout io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "document name")
	docType := fs.String("type", "", "document type: spec, plan, report, guide, decision")
	project := fs.String("project", "", "project name (defaults to canonical repository name)")
	title := fs.String("title", "", "optional document title")
	agent := fs.String("agent", cfg.SourceAgent, "source agent name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *docType == "" || fs.NArg() != 1 {
		return errors.New("usage: krakozhia save --name <name> --type <spec|plan|report|guide|decision> [--project P] [--title T] [--agent A] <file>")
	}
	proj := strings.TrimSpace(*project)
	if proj == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		homeDir, _ := os.UserHomeDir()
		proj = chats.ResolveProject(cwd, homeDir)
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}

	client := NewClient(cfg.URL, cfg.Token)
	res, err := client.SaveDocument(context.Background(), server.SaveDocumentRequest{
		Project:     proj,
		Name:        *name,
		DocType:     *docType,
		Title:       *title,
		Content:     string(raw),
		SourceAgent: *agent,
	})
	if err != nil {
		return err
	}
	verb := "saved"
	if res.Unchanged {
		verb = "unchanged"
	}
	fmt.Fprintf(stdout, "%s %s rev=%d sections=%d\n", verb, res.ID, res.Revision, res.ChunkCount)
	return nil
}

func runShow(args []string, stdout io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rev := fs.Int("revision", 0, "document revision")
	isNote := fs.Bool("note", false, "fetch a memory note instead of a document")
	if err := fs.Parse(escapeLeadingDashID(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: krakozhia show [--revision N] [--note] <id>")
	}

	client := NewClient(cfg.URL, cfg.Token)
	if *isNote {
		n, err := client.GetNote(context.Background(), fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, n.Content)
		if !strings.HasSuffix(n.Content, "\n") {
			fmt.Fprintln(stdout)
		}
		return nil
	}
	res, err := client.GetDocument(context.Background(), fs.Arg(0), *rev)
	if err != nil {
		return err
	}
	if res.Kind == "chunk" && res.Chunk != nil {
		if res.Chunk.HeadingPath != "" {
			fmt.Fprintf(stdout, "# %s\n\n", res.Chunk.HeadingPath)
		}
		fmt.Fprintln(stdout, res.Chunk.Content)
		return nil
	}
	if res.Document != nil {
		fmt.Fprint(stdout, res.Document.ContentMD)
		if !strings.HasSuffix(res.Document.ContentMD, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	return nil
}

func runList(args []string, stdout io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "filter by project")
	docType := fs.String("type", "", "filter by document type")
	limit := fs.Int("k", 50, "maximum documents")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := NewClient(cfg.URL, cfg.Token)
	docs, err := client.ListDocuments(context.Background(), *project, *docType, *limit)
	if err != nil {
		return err
	}
	for _, d := range docs {
		fmt.Fprintf(stdout, "%s\t%s\trev %d\t%s\t%s\n",
			d.ID, d.DocType, d.CurrentRevision, d.UpdatedAt.UTC().Format(time.DateOnly), d.Title)
	}
	return nil
}

func runBench(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: krakozhia bench <record|query|compare> [options]")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	client := NewClient(cfg.URL, cfg.Token)

	switch args[0] {
	case "record":
		fs := flag.NewFlagSet("bench record", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		project := fs.String("project", "", "project name")
		suite := fs.String("suite", "", "suite name")
		metric := fs.String("metric", "", "metric name")
		value := fs.Float64("value", 0, "metric value")
		unit := fs.String("unit", "", "metric unit")
		baseline := fs.Int64("baseline", 0, "optional baseline benchmark ID")
		delta := fs.String("delta", "", "optional percent delta override")
		commit := fs.String("commit", "", "git commit SHA")
		branch := fs.String("branch", "", "git branch")
		env := fs.String("env", "", "environment name")
		status := fs.String("status", "info", "baseline, kept, reverted, or info")
		notes := fs.String("notes", "", "run context notes")
		docID := fs.String("doc", "", "related document ID")
		agent := fs.String("agent", cfg.SourceAgent, "source agent")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *project == "" || *suite == "" || *metric == "" || *unit == "" {
			return errors.New("usage: krakozhia bench record --project P --suite S --metric M --value V --unit U [options]")
		}
		in := store.RecordBenchmarkInput{
			Project:     *project,
			Suite:       *suite,
			Metric:      *metric,
			Value:       *value,
			Unit:        *unit,
			GitCommit:   *commit,
			Branch:      *branch,
			Environment: *env,
			Status:      *status,
			Notes:       *notes,
			SourceAgent: *agent,
		}
		if *baseline > 0 {
			b := *baseline
			in.BaselineID = &b
		}
		if *delta != "" {
			var d float64
			if _, err := fmt.Sscanf(*delta, "%f", &d); err != nil {
				return fmt.Errorf("invalid --delta %q: %w", *delta, err)
			}
			in.DeltaPct = &d
		}
		if *docID != "" {
			d := *docID
			in.DocumentID = &d
		}
		b, err := client.RecordBenchmark(context.Background(), in)
		if err != nil {
			return err
		}
		deltaStr := "-"
		if b.DeltaPct != nil {
			deltaStr = fmt.Sprintf("%+.2f%%", *b.DeltaPct)
		}
		fmt.Fprintf(stdout, "id=%d project=%s suite=%s metric=%s value=%g %s delta=%s status=%s\n",
			b.ID, b.Project, b.Suite, b.Metric, b.Value, b.Unit, deltaStr, b.Status)
		return nil

	case "query":
		fs := flag.NewFlagSet("bench query", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		project := fs.String("project", "", "project filter")
		suite := fs.String("suite", "", "suite filter")
		metric := fs.String("metric", "", "metric filter")
		branch := fs.String("branch", "", "branch filter")
		commit := fs.String("commit", "", "commit filter")
		status := fs.String("status", "", "status filter")
		latest := fs.Bool("latest", false, "latest run per project/suite/metric")
		limit := fs.Int("k", 50, "maximum rows")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rows, err := client.QueryBenchmarks(context.Background(), store.BenchmarkFilter{
			Project:    *project,
			Suite:      *suite,
			Metric:     *metric,
			Branch:     *branch,
			GitCommit:  *commit,
			Status:     *status,
			LatestOnly: *latest,
			Limit:      *limit,
		})
		if err != nil {
			return err
		}
		printBenchRows(stdout, rows)
		return nil

	case "compare":
		fs := flag.NewFlagSet("bench compare", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		project := fs.String("project", "", "project name")
		suite := fs.String("suite", "", "suite name")
		metric := fs.String("metric", "", "metric name")
		limit := fs.Int("k", 50, "maximum rows")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *project == "" || *suite == "" || *metric == "" {
			return errors.New("usage: krakozhia bench compare --project P --suite S --metric M [-k N]")
		}
		rows, err := client.CompareBenchmarks(context.Background(), *project, *suite, *metric, *limit)
		if err != nil {
			return err
		}
		printBenchRows(stdout, rows)
		return nil

	default:
		return fmt.Errorf("unknown bench subcommand %q", args[0])
	}
}

func printBenchRows(stdout io.Writer, rows []store.Benchmark) {
	for _, b := range rows {
		deltaStr := "-"
		if b.DeltaPct != nil {
			deltaStr = fmt.Sprintf("%+.2f%%", *b.DeltaPct)
		}
		fmt.Fprintf(stdout, "#%d\t%s\t%s\t%s\t%g %s\t%s\t%s\t%s\t%s\n",
			b.ID, b.Project, b.Suite, b.Metric, b.Value, b.Unit, deltaStr, b.Status,
			b.RecordedAt.UTC().Format(time.DateOnly), b.Notes)
	}
}

func runBrief(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: krakozhia brief <project>")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	client := NewClient(cfg.URL, cfg.Token)
	b, err := client.GetBrief(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, b.BriefMD)
	if !strings.HasSuffix(b.BriefMD, "\n") {
		fmt.Fprintln(stdout)
	}
	return nil
}

func pruneOldSessions(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}

func loadManifest(path string) localManifest {
	raw, err := os.ReadFile(path)
	if err != nil {
		return localManifest{Hashes: map[string]string{}}
	}
	var m localManifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Hashes == nil {
		return localManifest{Hashes: map[string]string{}}
	}
	return m
}

func saveManifest(path string, localByID map[string]note.Note, failedIDs map[string]bool) error {
	m := localManifest{Hashes: make(map[string]string, len(localByID))}
	for id, n := range localByID {
		if failedIDs[id] {
			continue
		}
		m.Hashes[id] = n.ContentHash
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Note IDs for directories like /home/user begin with '-' and would be misparsed as flags by flag.FlagSet.
func escapeLeadingDashID(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && strings.Contains(a, "/") {
			out = append(out, "--")
			out = append(out, args[i:]...)
			break
		}
		out = append(out, a)
	}
	return out
}
