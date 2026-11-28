package server

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rustatian/krakozhia/internal/store"
)

type mcpSearchInput struct {
	Query      string `json:"query" jsonschema:"Search query text"`
	Kind       string `json:"kind,omitempty" jsonschema:"Filter by record kind: doc, note, or github"`
	Project    string `json:"project,omitempty" jsonschema:"Filter to one project or GitHub owner name"`
	Type       string `json:"type,omitempty" jsonschema:"Filter by document, note, or GitHub item type (issue, pr, discussion)"`
	Tag        string `json:"tag,omitempty" jsonschema:"Filter by derived tag or GitHub label"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum results to return (default 10)"`
	DedupeDocs bool   `json:"dedupe_docs,omitempty" jsonschema:"Keep only the highest-ranked chunk per document"`
	Rerank     bool   `json:"rerank,omitempty" jsonschema:"Rerank top results with the cross-encoder"`
}

type mcpGetDocInput struct {
	ID       string `json:"id" jsonschema:"Document ID (<project>/<name>) or section ID (<project>/<name>#<index>)"`
	Revision int    `json:"revision,omitempty" jsonschema:"Optional revision number; 0 returns the current revision"`
}

type mcpListDocsInput struct {
	Project string `json:"project,omitempty" jsonschema:"Optional project filter"`
	DocType string `json:"doc_type,omitempty" jsonschema:"Optional document type filter (roadmap, chat, spec, plan, report)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum documents to list (default 50)"`
}

type mcpListDocsOutput struct {
	Documents []store.Document `json:"documents"`
}

type mcpSaveNoteOutput struct {
	ID string `json:"id"`
}

type mcpGetNoteInput struct {
	ID string `json:"id" jsonschema:"Note ID (<project>/<file-stem>)"`
}

type mcpRecordBenchmarkInput struct {
	Project     string   `json:"project" jsonschema:"Project name"`
	Suite       string   `json:"suite" jsonschema:"Benchmark suite name"`
	Metric      string   `json:"metric" jsonschema:"Metric name"`
	Value       float64  `json:"value" jsonschema:"Measured numeric value"`
	Unit        string   `json:"unit" jsonschema:"Measurement unit (for example ms, req/s, GiB)"`
	BaselineID  *int64   `json:"baseline_id,omitempty" jsonschema:"Optional baseline benchmark row ID for automatic delta calculation"`
	DeltaPct    *float64 `json:"delta_pct,omitempty" jsonschema:"Optional percent change from baseline"`
	GitCommit   string   `json:"git_commit,omitempty" jsonschema:"Git commit SHA"`
	Branch      string   `json:"branch,omitempty" jsonschema:"Git branch name"`
	Environment string   `json:"environment,omitempty" jsonschema:"Host or cluster environment name"`
	Status      string   `json:"status,omitempty" jsonschema:"One of baseline, kept, reverted, or info"`
	Notes       string   `json:"notes,omitempty" jsonschema:"Short context for the run"`
	DocumentID  *string  `json:"document_id,omitempty" jsonschema:"Related document ID"`
	SourceAgent string   `json:"source_agent,omitempty" jsonschema:"Agent name writing the measurement"`
}

type mcpQueryBenchmarksInput struct {
	Project    string `json:"project,omitempty" jsonschema:"Filter by project"`
	Suite      string `json:"suite,omitempty" jsonschema:"Filter by suite"`
	Metric     string `json:"metric,omitempty" jsonschema:"Filter by metric"`
	Branch     string `json:"branch,omitempty" jsonschema:"Filter by branch"`
	GitCommit  string `json:"git_commit,omitempty" jsonschema:"Filter by git commit"`
	Status     string `json:"status,omitempty" jsonschema:"Filter by status (baseline, kept, reverted, info)"`
	LatestOnly bool   `json:"latest_only,omitempty" jsonschema:"Return only the newest run per project, suite, and metric"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum rows to return (default 50)"`
}

type mcpBenchmarksOutput struct {
	Benchmarks []store.Benchmark `json:"benchmarks"`
}

type mcpCompareBenchmarksInput struct {
	Project string `json:"project" jsonschema:"Project name"`
	Suite   string `json:"suite" jsonschema:"Suite name"`
	Metric  string `json:"metric" jsonschema:"Metric name"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum runs to compare (default 50)"`
}

type mcpGetBriefInput struct {
	Project string `json:"project" jsonschema:"Project name"`
}

func (s *Server) newMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "krakozhia",
		Version: s.version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_knowledge",
		Description: "Hybrid vector and full-text search across project documents, distilled chats, roadmaps, memory notes, and synced GitHub issues, pull requests, and discussions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, SearchResponse, error) {
		res, err := s.searchKnowledge(ctx, SearchRequest{
			Form:       store.FormHybrid,
			Query:      in.Query,
			Kind:       in.Kind,
			Project:    in.Project,
			Type:       in.Type,
			Tag:        in.Tag,
			Limit:      in.Limit,
			DedupeDocs: in.DedupeDocs,
			Rerank:     in.Rerank,
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_document",
		Description: "Fetch a full document revision or a single section chunk by ID.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpGetDocInput) (*mcp.CallToolResult, GetDocumentResponse, error) {
		res, err := s.getDocument(ctx, in.ID, in.Revision)
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_documents",
		Description: "List stored knowledge documents with optional project and type filters.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListDocsInput) (*mcp.CallToolResult, mcpListDocsOutput, error) {
		docs, err := s.store.ListDocuments(ctx, in.Project, in.DocType, in.Limit)
		if docs == nil {
			docs = []store.Document{}
		}
		return nil, mcpListDocsOutput{Documents: docs}, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_document",
		Description: "Save a Markdown design spec, plan, or report with revision tracking and secret scanning.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SaveDocumentRequest) (*mcp.CallToolResult, store.SaveDocumentResult, error) {
		res, err := s.saveDocument(ctx, in)
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "save_note",
		Description: "Create or update a searchable project or reference memory note.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SaveNoteRequest) (*mcp.CallToolResult, mcpSaveNoteOutput, error) {
		if err := s.saveNote(ctx, in); err != nil {
			return nil, mcpSaveNoteOutput{}, err
		}
		return nil, mcpSaveNoteOutput{ID: in.ID}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_note",
		Description: "Fetch a stored memory note by ID (<project>/<name>).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpGetNoteInput) (*mcp.CallToolResult, store.Note, error) {
		n, err := s.store.GetNote(ctx, in.ID)
		return nil, n, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "record_benchmark",
		Description: "Record a structured numeric benchmark or improvement measurement.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpRecordBenchmarkInput) (*mcp.CallToolResult, store.Benchmark, error) {
		b, err := s.recordBenchmark(ctx, store.RecordBenchmarkInput{
			Project:     in.Project,
			Suite:       in.Suite,
			Metric:      in.Metric,
			Value:       in.Value,
			Unit:        in.Unit,
			BaselineID:  in.BaselineID,
			DeltaPct:    in.DeltaPct,
			GitCommit:   in.GitCommit,
			Branch:      in.Branch,
			Environment: in.Environment,
			Status:      in.Status,
			Notes:       in.Notes,
			DocumentID:  in.DocumentID,
			SourceAgent: in.SourceAgent,
		})
		return nil, b, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "query_benchmarks",
		Description: "Query structured benchmark and improvement runs using exact SQL filters.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpQueryBenchmarksInput) (*mcp.CallToolResult, mcpBenchmarksOutput, error) {
		rows, err := s.store.QueryBenchmarks(ctx, store.BenchmarkFilter{
			Project:    in.Project,
			Suite:      in.Suite,
			Metric:     in.Metric,
			Branch:     in.Branch,
			GitCommit:  in.GitCommit,
			Status:     in.Status,
			LatestOnly: in.LatestOnly,
			Limit:      in.Limit,
		})
		if rows == nil {
			rows = []store.Benchmark{}
		}
		return nil, mcpBenchmarksOutput{Benchmarks: rows}, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "compare_benchmarks",
		Description: "List chronological runs for one project, suite, and metric to compare changes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCompareBenchmarksInput) (*mcp.CallToolResult, mcpBenchmarksOutput, error) {
		if strings.TrimSpace(in.Project) == "" || strings.TrimSpace(in.Suite) == "" || strings.TrimSpace(in.Metric) == "" {
			return nil, mcpBenchmarksOutput{}, badRequestError{msg: "project, suite, and metric are required"}
		}
		rows, err := s.store.CompareBenchmarks(ctx, in.Project, in.Suite, in.Metric, in.Limit)
		if rows == nil {
			rows = []store.Benchmark{}
		}
		return nil, mcpBenchmarksOutput{Benchmarks: rows}, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_project_brief",
		Description: "Fetch the auto-maintained Markdown brief for a project, including architecture, planned GitHub roadmap and future extension points, recent chat outcomes, and memory notes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpGetBriefInput) (*mcp.CallToolResult, store.ProjectBrief, error) {
		if strings.TrimSpace(in.Project) == "" {
			return nil, store.ProjectBrief{}, badRequestError{msg: "project is required"}
		}
		b, err := s.store.GetProjectBrief(ctx, in.Project)
		return nil, b, err
	})

	return srv
}
