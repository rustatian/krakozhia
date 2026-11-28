// Package webui renders a read-only HTML browser for documents, notes, briefs, and benchmarks.
package webui

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/store"
)

// Reader provides read-only queries required by the HTML UI.
type Reader interface {
	Stats(ctx context.Context) (store.StoreStats, error)
	ListProjects(ctx context.Context) ([]string, error)
	ListDocuments(ctx context.Context, project, docType string, limit int) ([]store.Document, error)
	GetDocument(ctx context.Context, id string, revision int) (store.Document, error)
	ListChunks(ctx context.Context, docID string) ([]store.Chunk, error)
	ListNotes(ctx context.Context, project, noteType string, limit int) ([]store.Note, error)
	GetNote(ctx context.Context, id string) (store.Note, error)
	QueryBenchmarks(ctx context.Context, filter store.BenchmarkFilter) ([]store.Benchmark, error)
	GetProjectBrief(ctx context.Context, project string) (store.ProjectBrief, error)
	ListRecordLinks(ctx context.Context, targetKind, targetID string) ([]store.RecordLink, error)
	ListReposForProject(ctx context.Context, project string) ([]store.GitHubRepo, error)
	ListGitHubItems(ctx context.Context, filter store.GitHubItemFilter) ([]store.GitHubItem, error)
}

// SearchParams configures a UI search query.
type SearchParams struct {
	Query   string
	Project string
	Kind    string
}

// SearchFunc runs a search query for the Web UI.
type SearchFunc func(ctx context.Context, params SearchParams) ([]store.SearchHit, bool, error)

// UI serves read-only HTML pages for browsing knowledge base records.
type UI struct {
	reader Reader
	search SearchFunc
	mux    *http.ServeMux
	tmpl   *template.Template
}

// New builds a read-only Web UI handler.
func New(reader Reader, search SearchFunc) http.Handler {
	funcs := template.FuncMap{
		"fmtDate": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.UTC().Format(time.DateOnly)
		},
		"fmtDelta": func(d *float64) string {
			if d == nil {
				return "-"
			}
			return fmt.Sprintf("%+.2f%%", *d)
		},
		"urlEscape":  url.QueryEscape,
		"pathEscape": url.PathEscape,
	}
	tmpl := template.Must(template.New("ui").Funcs(funcs).Parse(uiTemplates))
	u := &UI{
		reader: reader,
		search: search,
		mux:    http.NewServeMux(),
		tmpl:   tmpl,
	}
	u.mux.HandleFunc("GET /{$}", u.handleHome)
	u.mux.HandleFunc("GET /roadmaps", u.handleRoadmaps)
	u.mux.HandleFunc("GET /documents", u.handleDocuments)
	u.mux.HandleFunc("GET /notes", u.handleNotes)
	u.mux.HandleFunc("GET /search", u.handleSearch)
	u.mux.HandleFunc("GET /projects/{project}", u.handleProject)
	u.mux.HandleFunc("GET /document", u.handleDocument)
	u.mux.HandleFunc("GET /note", u.handleNote)
	u.mux.HandleFunc("GET /benchmarks", u.handleBenchmarks)
	return u
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u.mux.ServeHTTP(w, r)
}

func (u *UI) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := u.reader.Stats(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	projects, err := u.reader.ListProjects(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	docs, err := u.reader.ListDocuments(ctx, "", "", 15)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	notes, err := u.reader.ListNotes(ctx, "", "", 15)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "home", map[string]any{
		"Title":     "krakozhia knowledge base",
		"Stats":     stats,
		"Projects":  projects,
		"Documents": docs,
		"Notes":     notes,
	})
}

func (u *UI) handleRoadmaps(w http.ResponseWriter, r *http.Request) {
	docs, err := u.reader.ListDocuments(r.Context(), "", "roadmap", 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "roadmaps", map[string]any{
		"Title":    "Roadmaps and Directions - krakozhia",
		"Roadmaps": docs,
	})
}

func (u *UI) handleDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := strings.TrimSpace(q.Get("project"))
	docType := strings.TrimSpace(q.Get("type"))
	docs, err := u.reader.ListDocuments(r.Context(), project, docType, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "documents", map[string]any{
		"Title":     "Documents and Chats - krakozhia",
		"Project":   project,
		"DocType":   docType,
		"Documents": docs,
	})
}

func (u *UI) handleNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := strings.TrimSpace(q.Get("project"))
	noteType := strings.TrimSpace(q.Get("type"))
	notes, err := u.reader.ListNotes(r.Context(), project, noteType, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "notes", map[string]any{
		"Title":    "Memory Notes - krakozhia",
		"Project":  project,
		"NoteType": noteType,
		"Notes":    notes,
	})
}

func (u *UI) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))

	var hits []store.SearchHit
	var degraded bool
	if q != "" && u.search != nil {
		var err error
		hits, degraded, err = u.search(r.Context(), SearchParams{
			Query:   q,
			Project: project,
			Kind:    kind,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	u.render(w, "search", map[string]any{
		"Title":    "Search - krakozhia",
		"Query":    q,
		"Project":  project,
		"Kind":     kind,
		"Degraded": degraded,
		"Hits":     hits,
	})
}

func (u *UI) handleProject(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if project == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var brief *store.ProjectBrief
	if b, err := u.reader.GetProjectBrief(ctx, project); err == nil {
		brief = &b
	} else if !errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	docs, err := u.reader.ListDocuments(ctx, project, "", 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	notes, err := u.reader.ListNotes(ctx, project, "", 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	benches, err := u.reader.QueryBenchmarks(ctx, store.BenchmarkFilter{Project: project, LatestOnly: true, Limit: 30})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repos, err := u.reader.ListReposForProject(ctx, project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ghItems, err := u.reader.ListGitHubItems(ctx, store.GitHubItemFilter{Project: project, State: "open", Limit: 30})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if brief == nil && len(docs) == 0 && len(notes) == 0 && len(benches) == 0 && len(repos) == 0 && len(ghItems) == 0 {
		http.NotFound(w, r)
		return
	}
	u.render(w, "project", map[string]any{
		"Title":       project + " - krakozhia",
		"Project":     project,
		"Brief":       brief,
		"Documents":   docs,
		"Notes":       notes,
		"Benchmarks":  benches,
		"GitHubRepos": repos,
		"GitHubItems": ghItems,
	})
}

func (u *UI) handleDocument(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	rev := 0
	if raw := r.URL.Query().Get("revision"); raw != "" {
		rev, _ = strconv.Atoi(raw)
	}
	ctx := r.Context()
	doc, err := u.reader.GetDocument(ctx, id, rev)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	chunks, err := u.reader.ListChunks(ctx, doc.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	links, err := u.reader.ListRecordLinks(ctx, "doc", doc.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "document", map[string]any{
		"Title":    doc.Title + " - krakozhia",
		"Document": doc,
		"Chunks":   chunks,
		"Links":    links,
	})
}

func (u *UI) handleNote(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	n, err := u.reader.GetNote(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "note", map[string]any{
		"Title": n.Name + " - krakozhia",
		"Note":  n,
	})
}

func (u *UI) handleBenchmarks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := u.reader.QueryBenchmarks(r.Context(), store.BenchmarkFilter{
		Project: q.Get("project"),
		Suite:   q.Get("suite"),
		Metric:  q.Get("metric"),
		Status:  q.Get("status"),
		Limit:   100,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "benchmarks", map[string]any{
		"Title":      "Benchmarks - krakozhia",
		"Project":    q.Get("project"),
		"Suite":      q.Get("suite"),
		"Metric":     q.Get("metric"),
		"Benchmarks": rows,
	})
}

func (u *UI) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

const uiTemplates = `
{{define "head"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light dark; font-family: system-ui, -apple-system, sans-serif; }
  body { max-width: 1100px; margin: 0 auto; padding: 1.5rem; line-height: 1.5; }
  header { display: flex; flex-wrap: wrap; align-items: center; gap: 1rem; border-bottom: 1px solid #8884; padding-bottom: 0.75rem; margin-bottom: 1.5rem; }
  header a { text-decoration: none; font-weight: 600; }
  nav { display: flex; gap: 1rem; margin-right: auto; }
  form.search { display: flex; gap: 0.5rem; }
  input, select, button { padding: 0.35rem 0.6rem; font: inherit; }
  table { width: 100%; border-collapse: collapse; margin: 1rem 0; font-size: 0.92rem; }
  th, td { text-align: left; padding: 0.45rem 0.6rem; border-bottom: 1px solid #8883; vertical-align: top; }
  pre { padding: 0.9rem; border: 1px solid #8884; border-radius: 6px; overflow-x: auto; white-space: pre-wrap; word-break: break-word; }
  .stats { display: flex; flex-wrap: wrap; gap: 1rem; margin-bottom: 1.5rem; }
  .stat { border: 1px solid #8884; border-radius: 6px; padding: 0.6rem 0.9rem; }
  .muted { opacity: 0.75; font-size: 0.9rem; }
  .tag { display: inline-block; border: 1px solid #8886; border-radius: 4px; padding: 0.1rem 0.4rem; margin-right: 0.3rem; font-size: 0.85rem; }
</style>
</head>
<body>
<header>
  <a href="/">krakozhia</a>
  <nav>
    <a href="/">Overview</a>
    <a href="/roadmaps">Roadmaps</a>
    <a href="/documents">Documents</a>
    <a href="/notes">Notes</a>
  </nav>
  <form class="search" action="/search" method="get">
    <input type="search" name="q" placeholder="Search chats, docs, notes, and tickets..." required>
    <button type="submit">Search</button>
  </form>
</header>
{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "home"}}
{{template "head" .}}
<div class="stats">
  <div class="stat"><strong>{{len .Projects}}</strong> projects</div>
  <div class="stat"><strong>{{.Stats.Documents}}</strong> documents &amp; chats</div>
  <div class="stat"><strong>{{.Stats.Notes}}</strong> notes</div>
  {{if .Stats.GitHubItems}}<div class="stat"><strong>{{.Stats.GitHubItems}}</strong> GitHub items</div>{{end}}
  <div class="stat">jobs pending: <strong>{{index .Stats.JobsByStatus "pending"}}</strong></div>
</div>

<h2>Projects</h2>
{{if .Projects}}
<ul>
{{range .Projects}}<li><a href="/projects/{{pathEscape .}}">{{.}}</a></li>{{end}}
</ul>
{{else}}<p class="muted">No projects indexed yet.</p>{{end}}

<h2>Recent Documents and Chats</h2>
{{if .Documents}}
<table>
  <thead><tr><th>ID</th><th>Type</th><th>Title</th><th>Summary</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Documents}}
    <tr>
      <td><a href="/document?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td>{{.DocType}}</td>
      <td>{{.Title}}</td>
      <td class="muted">{{.Summary}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No documents or chats saved yet.</p>{{end}}

<h2>Recent Memory Notes</h2>
{{if .Notes}}
<table>
  <thead><tr><th>ID</th><th>Project</th><th>Type</th><th>Description</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Notes}}
    <tr>
      <td><a href="/note?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td><a href="/projects/{{pathEscape .Project}}">{{.Project}}</a></td>
      <td>{{.NoteType}}</td>
      <td class="muted">{{if .Summary}}{{.Summary}}{{else}}{{.Description}}{{end}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No notes synced yet.</p>{{end}}
{{template "foot" .}}
{{end}}

{{define "roadmaps"}}
{{template "head" .}}
<h1>Roadmaps and Directions</h1>
{{if .Roadmaps}}
<table>
  <thead><tr><th>ID</th><th>Project / Org</th><th>Title</th><th>Summary</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Roadmaps}}
    <tr>
      <td><a href="/document?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td><a href="/projects/{{pathEscape .Project}}">{{.Project}}</a></td>
      <td>{{.Title}}</td>
      <td class="muted">{{.Summary}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No roadmaps synthesized yet.</p>{{end}}
{{template "foot" .}}
{{end}}

{{define "documents"}}
{{template "head" .}}
<h1>Documents and Chats</h1>
<form class="search" action="/documents" method="get">
  <input type="text" name="project" value="{{.Project}}" placeholder="Project">
  <input type="text" name="type" value="{{.DocType}}" placeholder="Type (chat, spec, plan, report)">
  <button type="submit">Filter</button>
</form>
{{if .Documents}}
<table>
  <thead><tr><th>ID</th><th>Project</th><th>Type</th><th>Rev</th><th>Title</th><th>Summary</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Documents}}
    <tr>
      <td><a href="/document?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td><a href="/projects/{{pathEscape .Project}}">{{.Project}}</a></td>
      <td>{{.DocType}}</td>
      <td>{{.CurrentRevision}}</td>
      <td>{{.Title}}</td>
      <td class="muted">{{.Summary}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No matching documents.</p>{{end}}
{{template "foot" .}}
{{end}}

{{define "notes"}}
{{template "head" .}}
<h1>Memory Notes</h1>
<form class="search" action="/notes" method="get">
  <input type="text" name="project" value="{{.Project}}" placeholder="Project">
  <input type="text" name="type" value="{{.NoteType}}" placeholder="Type (user, feedback, project, reference)">
  <button type="submit">Filter</button>
</form>
{{if .Notes}}
<table>
  <thead><tr><th>ID</th><th>Project</th><th>Type</th><th>Name</th><th>Description</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Notes}}
    <tr>
      <td><a href="/note?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td><a href="/projects/{{pathEscape .Project}}">{{.Project}}</a></td>
      <td>{{.NoteType}}</td>
      <td>{{.Name}}</td>
      <td class="muted">{{if .Summary}}{{.Summary}}{{else}}{{.Description}}{{end}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No matching notes.</p>{{end}}
{{template "foot" .}}
{{end}}

{{define "search"}}
{{template "head" .}}
<h1>Search{{if .Query}}: {{.Query}}{{end}}</h1>
{{if .Degraded}}<p class="muted">Vector search unavailable; showing full-text results.</p>{{end}}
{{if .Hits}}
<table>
  <thead><tr><th>Kind</th><th>ID</th><th>Title</th><th>Snippet</th></tr></thead>
  <tbody>
  {{range .Hits}}
    <tr>
      <td>{{.Kind}}</td>
      <td>
        {{if eq .Kind "doc"}}<a href="/document?id={{urlEscape .DocumentID}}">{{.ID}}</a>
        {{else if eq .Kind "github"}}{{if .URL}}<a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.ID}}</a>{{else}}{{.ID}}{{end}}
        {{else}}<a href="/note?id={{urlEscape .ID}}">{{.ID}}</a>{{end}}
      </td>
      <td>{{.Title}}{{if .HeadingPath}} <span class="muted">({{.HeadingPath}})</span>{{end}}</td>
      <td class="muted">{{.Snippet}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No matching records.</p>{{end}}
{{template "foot" .}}
{{end}}

{{define "project"}}
{{template "head" .}}
<h1>Project: {{.Project}}</h1>
{{if .Brief}}
<h2>Project Brief <span class="muted">(updated {{fmtDate .Brief.UpdatedAt}})</span></h2>
<pre>{{.Brief.BriefMD}}</pre>
{{end}}

{{if .GitHubRepos}}
<h2>Linked GitHub Repositories</h2>
<ul>
{{range .GitHubRepos}}<li><code>{{.FullName}}</code>{{if .Description}} - <span class="muted">{{.Description}}</span>{{end}}</li>{{end}}
</ul>
{{end}}

{{if .GitHubItems}}
<h2>Open GitHub Issues, PRs, and Discussions</h2>
<table>
  <thead><tr><th>Ref</th><th>Kind</th><th>Title</th><th>Milestone</th><th>Sub-issues</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .GitHubItems}}
    <tr>
      <td>{{if .URL}}<a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.RepoFullName}}#{{.Number}}</a>{{else}}{{.RepoFullName}}#{{.Number}}{{end}}</td>
      <td>{{.Kind}} ({{.State}})</td>
      <td>{{.Title}}</td>
      <td class="muted">{{.MilestoneTitle}}</td>
      <td class="muted">{{if .SubIssuesTotal}}{{.SubIssuesCompleted}}/{{.SubIssuesTotal}}{{else}}-{{end}}</td>
      <td>{{fmtDate .GitHubUpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{end}}

<h2>Documents</h2>
{{if .Documents}}
<table>
  <thead><tr><th>ID</th><th>Type</th><th>Rev</th><th>Title</th><th>Tags</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Documents}}
    <tr>
      <td><a href="/document?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td>{{.DocType}}</td>
      <td>{{.CurrentRevision}}</td>
      <td>{{.Title}}</td>
      <td>{{range .Tags}}<span class="tag">{{.}}</span>{{end}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No documents.</p>{{end}}

<h2>Memory Notes</h2>
{{if .Notes}}
<table>
  <thead><tr><th>ID</th><th>Type</th><th>Description</th><th>Updated</th></tr></thead>
  <tbody>
  {{range .Notes}}
    <tr>
      <td><a href="/note?id={{urlEscape .ID}}">{{.ID}}</a></td>
      <td>{{.NoteType}}</td>
      <td class="muted">{{.Description}}</td>
      <td>{{fmtDate .UpdatedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No notes.</p>{{end}}

{{if .Benchmarks}}
<h2>Latest Benchmarks</h2>
<table>
  <thead><tr><th>Suite</th><th>Metric</th><th>Value</th><th>Delta</th><th>Status</th><th>Commit</th><th>Notes</th></tr></thead>
  <tbody>
  {{range .Benchmarks}}
    <tr>
      <td>{{.Suite}}</td>
      <td>{{.Metric}}</td>
      <td>{{.Value}} {{.Unit}}</td>
      <td>{{fmtDelta .DeltaPct}}</td>
      <td>{{.Status}}</td>
      <td>{{.GitCommit}}</td>
      <td class="muted">{{.Notes}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{end}}
{{template "foot" .}}
{{end}}

{{define "document"}}
{{template "head" .}}
<h1>{{.Document.Title}}</h1>
<p class="muted">
  ID: <code>{{.Document.ID}}</code> | Type: {{.Document.DocType}} | Revision: {{.Document.Revision}} / {{.Document.CurrentRevision}} | Agent: {{.Document.SourceAgent}} | Updated: {{fmtDate .Document.UpdatedAt}}
</p>
{{if .Document.Summary}}<p><strong>Summary:</strong> {{.Document.Summary}}</p>{{end}}
{{if .Document.Tags}}<p>{{range .Document.Tags}}<span class="tag">{{.}}</span>{{end}}</p>{{end}}

{{if .Links}}
<h2>Advisory Links</h2>
<ul>
{{range .Links}}<li>{{.Relation}}: <a href="/document?id={{urlEscape .SourceID}}">{{.SourceID}}</a> ({{.Note}})</li>{{end}}
</ul>
{{end}}

<h2>Content</h2>
<pre>{{.Document.ContentMD}}</pre>

{{if .Chunks}}
<h2>Sections</h2>
<table>
  <thead><tr><th>#</th><th>Heading</th><th>Context Prefix</th></tr></thead>
  <tbody>
  {{range .Chunks}}
    <tr><td>{{.SectionIndex}}</td><td>{{.HeadingPath}}</td><td class="muted">{{.ContextPrefix}}</td></tr>
  {{end}}
  </tbody>
</table>
{{end}}
{{template "foot" .}}
{{end}}

{{define "note"}}
{{template "head" .}}
<h1>{{.Note.Name}}</h1>
<p class="muted">ID: <code>{{.Note.ID}}</code> | Project: <a href="/projects/{{pathEscape .Note.Project}}">{{.Note.Project}}</a> | Type: {{.Note.NoteType}} | Updated: {{fmtDate .Note.UpdatedAt}}</p>
{{if .Note.Summary}}<p><strong>Summary:</strong> {{.Note.Summary}}</p>{{end}}
{{if .Note.Tags}}<p>{{range .Note.Tags}}<span class="tag">{{.}}</span>{{end}}</p>{{end}}
<pre>{{.Note.Content}}</pre>
{{template "foot" .}}
{{end}}

{{define "benchmarks"}}
{{template "head" .}}
<h1>Benchmarks</h1>
<form class="search" action="/benchmarks" method="get">
  <input type="text" name="project" value="{{.Project}}" placeholder="Project">
  <input type="text" name="suite" value="{{.Suite}}" placeholder="Suite">
  <input type="text" name="metric" value="{{.Metric}}" placeholder="Metric">
  <button type="submit">Filter</button>
</form>
{{if .Benchmarks}}
<table>
  <thead><tr><th>ID</th><th>Project</th><th>Suite</th><th>Metric</th><th>Value</th><th>Delta</th><th>Status</th><th>Commit</th><th>Branch</th><th>Env</th><th>Notes</th><th>Date</th></tr></thead>
  <tbody>
  {{range .Benchmarks}}
    <tr>
      <td>{{.ID}}</td>
      <td><a href="/projects/{{pathEscape .Project}}">{{.Project}}</a></td>
      <td>{{.Suite}}</td>
      <td>{{.Metric}}</td>
      <td>{{.Value}} {{.Unit}}</td>
      <td>{{fmtDelta .DeltaPct}}</td>
      <td>{{.Status}}</td>
      <td>{{.GitCommit}}</td>
      <td>{{.Branch}}</td>
      <td>{{.Environment}}</td>
      <td class="muted">{{.Notes}}</td>
      <td>{{fmtDate .RecordedAt}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}<p class="muted">No matching benchmarks.</p>{{end}}
{{template "foot" .}}
{{end}}
`
