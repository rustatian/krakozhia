package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/store"
)

func TestParseGitHubRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		in        string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{name: "standard owner and repo", in: "rapira-rs/rapira", wantOwner: "rapira-rs", wantName: "rapira", wantOK: true},
		{name: "strips trailing .git", in: "rustatian/krakozhia.git", wantOwner: "rustatian", wantName: "krakozhia", wantOK: true},
		{name: "preserves dots and hyphens", in: "rapira-rs/rapira-rs.github.io", wantOwner: "rapira-rs", wantName: "rapira-rs.github.io", wantOK: true},
		{name: "dot repo name allowed", in: "rapira-rs/.github", wantOwner: "rapira-rs", wantName: ".github", wantOK: true},
		{name: "missing slash rejected", in: "krakozhia", wantOK: false},
		{name: "extra path segment rejected", in: "rustatian/krakozhia/extra", wantOK: false},
		{name: "whitespace rejected", in: "rustatian/kra kozhia", wantOK: false},
		{name: "empty rejected", in: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, name, ok := store.ParseGitHubRepo(tc.in)
			if ok != tc.wantOK || owner != tc.wantOwner || name != tc.wantName {
				t.Fatalf("ParseGitHubRepo(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, owner, name, ok, tc.wantOwner, tc.wantName, tc.wantOK)
			}
		})
	}
}

func TestGitHubRoadmapStoreLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	pushed := now.Add(-1 * time.Hour)
	if err := s.UpsertGitHubRepo(ctx, store.GitHubRepo{
		FullName:       "rapira-rs/rapira",
		Owner:          "rapira-rs",
		Name:           "rapira",
		Project:        "rapira",
		Description:    "High-performance PHP application server in Rust",
		IsPrivate:      false,
		HasDiscussions: false,
		PushedAt:       &pushed,
		SyncHash:       "hash-v1",
	}); err != nil {
		t.Fatalf("UpsertGitHubRepo: %v", err)
	}

	// Register local workspace project alias "core" -> "rapira-rs/rapira".
	if err := s.UpsertProjectRepo(ctx, "core", "rapira-rs/rapira"); err != nil {
		t.Fatalf("UpsertProjectRepo: %v", err)
	}

	repos, err := s.ListReposForProject(ctx, "core")
	if err != nil || len(repos) != 1 || repos[0].FullName != "rapira-rs/rapira" {
		t.Fatalf("ListReposForProject(core) = %+v, err=%v", repos, err)
	}
	owners, err := s.ListOwnersForProject(ctx, "core")
	if err != nil || !slices.Equal(owners, []string{"rapira-rs"}) {
		t.Fatalf("ListOwnersForProject(core) = %v, err=%v", owners, err)
	}
	projects, err := s.ListProjectsForOwner(ctx, "rapira-rs")
	if err != nil || !slices.Contains(projects, "core") || !slices.Contains(projects, "rapira") {
		t.Fatalf("ListProjectsForOwner(rapira-rs) = %v, err=%v", projects, err)
	}

	due := now.Add(30 * 24 * time.Hour)
	if err := s.UpsertGitHubMilestones(ctx, "rapira-rs/rapira", []store.GitHubMilestone{
		{
			RepoFullName: "rapira-rs/rapira",
			Number:       1,
			Title:        "v1.0",
			Description:  "Initial stable runtime with queues and streams",
			State:        "open",
			DueOn:        &due,
			OpenIssues:   12,
			ClosedIssues: 25,
			UpdatedAt:    now,
		},
	}); err != nil {
		t.Fatalf("UpsertGitHubMilestones: %v", err)
	}
	milestones, err := s.ListGitHubMilestones(ctx, "rapira-rs/rapira")
	if err != nil || len(milestones) != 1 || milestones[0].Title != "v1.0" {
		t.Fatalf("ListGitHubMilestones = %+v, err=%v", milestones, err)
	}

	parentNum := 33
	item := store.GitHubItem{
		ID:                 "rapira-rs/rapira#issue:37",
		RepoFullName:       "rapira-rs/rapira",
		Owner:              "rapira-rs",
		Project:            "rapira",
		Kind:               "issue",
		Number:             37,
		State:              "open",
		IssueType:          "Feature",
		Title:              "Unified transport interface for HTTP, gRPC, and message brokers",
		Body:               "Design core transport trait so future AMQP, Kafka, and stream plugins fit without breaking changes.",
		Labels:             []string{"enhancement", "architecture"},
		MilestoneTitle:     "v1.0",
		ParentNumber:       &parentNum,
		SubIssuesTotal:     8,
		SubIssuesCompleted: 5,
		URL:                "https://github.com/rapira-rs/rapira/issues/37",
		GitHubCreatedAt:    now.Add(-48 * time.Hour),
		GitHubUpdatedAt:    now,
	}
	changed, err := s.UpsertGitHubItem(ctx, item)
	if err != nil || !changed {
		t.Fatalf("first UpsertGitHubItem = (%v, %v), want (true, nil)", changed, err)
	}

	// Second identical upsert must report unchanged.
	changed, err = s.UpsertGitHubItem(ctx, item)
	if err != nil || changed {
		t.Fatalf("second UpsertGitHubItem = (%v, %v), want (false, nil)", changed, err)
	}

	// Verify embed_github_item job was enqueued.
	job, err := s.ClaimNextJob(ctx)
	if err != nil || job == nil || job.JobType != "embed_github_item" || job.TargetID != item.ID {
		t.Fatalf("ClaimNextJob = %+v, err=%v", job, err)
	}
	if err := s.UpdateGitHubItemEmbedding(ctx, item.ID, unitVec(1024, 3, 1.0)); err != nil {
		t.Fatalf("UpdateGitHubItemEmbedding: %v", err)
	}
	if err := s.CompleteJob(ctx, job.ID); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	got, err := s.GetGitHubItem(ctx, item.ID)
	if err != nil || got.SubIssuesTotal != 8 || got.ParentNumber == nil || *got.ParentNumber != 33 {
		t.Fatalf("GetGitHubItem = %+v, err=%v", got, err)
	}

	listed, err := s.ListGitHubItems(ctx, store.GitHubItemFilter{Owner: "rapira-rs", State: "open"})
	if err != nil || len(listed) != 1 || listed[0].ID != item.ID {
		t.Fatalf("ListGitHubItems = %+v, err=%v", listed, err)
	}

	// Verify hybrid search over github_items works through project alias ("core" -> "rapira-rs/rapira").
	hits, err := s.SearchKnowledge(ctx, store.SearchQuery{
		Form:        store.FormHybrid,
		Text:        "AMQP Kafka broker transport trait",
		QueryVector: unitVec(1024, 3, 1.0),
		Kind:        "github",
		Project:     "core",
		Limit:       5,
	})
	if err != nil || len(hits) != 1 || hits[0].ID != item.ID || hits[0].URL != item.URL {
		t.Fatalf("SearchKnowledge(github, project=core) = %+v, err=%v", hits, err)
	}

	// Verify UpsertChatSession registers project_repos from RepoFullName.
	if _, err := s.UpsertChatSession(ctx, store.IngestChatInput{
		SourceAgent:  "claude",
		ExternalID:   "chat-remote-1",
		Project:      "core-wt",
		RepoFullName: "rapira-rs/rapira",
		Title:        "Transport design",
		RawExcerpt:   "User: Design transport traits.\nAssistant: Keep request envelopes transport-agnostic.",
	}); err != nil {
		t.Fatalf("UpsertChatSession: %v", err)
	}
	wtRepos, err := s.ListReposForProject(ctx, "core-wt")
	if err != nil || len(wtRepos) != 1 || wtRepos[0].FullName != "rapira-rs/rapira" {
		t.Fatalf("ListReposForProject(core-wt) = %+v, err=%v", wtRepos, err)
	}
}
