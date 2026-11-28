package githubsync_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/githubsync"
	"github.com/rustatian/krakozhia/internal/secrets"
	"github.com/rustatian/krakozhia/internal/store"
)

func TestParseOwnerConfig(t *testing.T) {
	t.Parallel()

	ownerCases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "trims and deduplicates owners", raw: " roadrunner-server, rapira-rs , rustatian, RAPIRA-RS ", want: []string{"roadrunner-server", "rapira-rs", "rustatian"}},
		{name: "empty string returns nil", raw: "   ", want: nil},
	}
	for _, tc := range ownerCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := githubsync.ParseOwners(tc.raw)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ParseOwners(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}

	tokCases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "parses comma separated owner=token pairs",
			raw:  "Rapira-RS=tok-rapira, roadrunner-server=tok-rr, rustatian=tok-me",
			want: map[string]string{"rapira-rs": "tok-rapira", "roadrunner-server": "tok-rr", "rustatian": "tok-me"},
		},
		{
			name: "skips malformed entries without equals",
			raw:  "badentry, rustatian=tok=with=eq",
			want: map[string]string{"rustatian": "tok=with=eq"},
		},
	}
	for _, tc := range tokCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := githubsync.ParseOwnerTokens(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseOwnerTokens(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("token[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

type fakeSyncStore struct {
	repos      map[string]store.GitHubRepo
	milestones map[string][]store.GitHubMilestone
	items      map[string]store.GitHubItem
	projByRepo map[string][]string
	jobs       []string
}

func newFakeSyncStore() *fakeSyncStore {
	return &fakeSyncStore{
		repos:      map[string]store.GitHubRepo{},
		milestones: map[string][]store.GitHubMilestone{},
		items:      map[string]store.GitHubItem{},
		projByRepo: map[string][]string{},
	}
}

func (f *fakeSyncStore) GetGitHubRepo(_ context.Context, fullName string) (store.GitHubRepo, error) {
	r, ok := f.repos[fullName]
	if !ok {
		return store.GitHubRepo{}, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeSyncStore) UpsertGitHubRepo(_ context.Context, repo store.GitHubRepo) error {
	f.repos[repo.FullName] = repo
	return nil
}

func (f *fakeSyncStore) UpsertGitHubMilestones(_ context.Context, repoFullName string, ms []store.GitHubMilestone) error {
	f.milestones[repoFullName] = ms
	return nil
}

func (f *fakeSyncStore) UpsertGitHubItem(_ context.Context, item store.GitHubItem) (bool, error) {
	prev, exists := f.items[item.ID]
	f.items[item.ID] = item
	return !exists || prev.ContentHash != item.ContentHash, nil
}

func (f *fakeSyncStore) ListProjectsForRepo(_ context.Context, repoFullName string) ([]string, error) {
	if list, ok := f.projByRepo[repoFullName]; ok {
		return list, nil
	}
	_, name, ok := store.ParseGitHubRepo(repoFullName)
	if !ok {
		return nil, nil
	}
	return []string{name}, nil
}

func (f *fakeSyncStore) EnqueueJob(_ context.Context, jobType, targetKind, targetID string) error {
	f.jobs = append(f.jobs, jobType+":"+targetKind+":"+targetID)
	return nil
}

func TestSyncAllGraphQLAndIdempotency(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	recentClosed := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	ancientClosed := now.Add(-200 * 24 * time.Hour).Format(time.RFC3339)
	recentOpen := now.Add(-2 * time.Hour).Format(time.RFC3339)
	secretTok := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"

	seenAuth := map[string]string{}
	gqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		owner, _ := req.Variables["owner"].(string)
		seenAuth[owner] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "repositoryOwner"):
			switch owner {
			case "rapira-rs":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repositoryOwner": map[string]any{
							"repositories": map[string]any{
								"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
								"nodes": []map[string]any{
									{
										"name":                  "rapira",
										"nameWithOwner":         "rapira-rs/rapira",
										"description":           "Rust PHP application server",
										"isPrivate":             false,
										"isArchived":            false,
										"hasDiscussionsEnabled": true,
										"pushedAt":              recentOpen,
										"issues":                map[string]any{"totalCount": 1},
										"pullRequests":          map[string]any{"totalCount": 1},
									},
									{
										"name":                  "old-archived",
										"nameWithOwner":         "rapira-rs/old-archived",
										"description":           "Never tracked archived repo",
										"isPrivate":             false,
										"isArchived":            true,
										"hasDiscussionsEnabled": false,
										"pushedAt":              ancientClosed,
										"issues":                map[string]any{"totalCount": 0},
										"pullRequests":          map[string]any{"totalCount": 0},
									},
									{
										"name":                  "newly-archived",
										"nameWithOwner":         "rapira-rs/newly-archived",
										"description":           "Previously tracked repo now archived",
										"isPrivate":             false,
										"isArchived":            true,
										"hasDiscussionsEnabled": false,
										"pushedAt":              ancientClosed,
										"issues":                map[string]any{"totalCount": 0},
										"pullRequests":          map[string]any{"totalCount": 0},
									},
								},
							},
						},
					},
				})
			case "rustatian":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repositoryOwner": map[string]any{
							"repositories": map[string]any{
								"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
								"nodes": []map[string]any{
									{
										"name":                  "krakozhia",
										"nameWithOwner":         "rustatian/krakozhia",
										"description":           "Self-hosted AI knowledge base",
										"isPrivate":             false,
										"isArchived":            false,
										"hasDiscussionsEnabled": false,
										"pushedAt":              recentOpen,
										"issues":                map[string]any{"totalCount": 0},
										"pullRequests":          map[string]any{"totalCount": 0},
									},
								},
							},
						},
					},
				})
			}

		case strings.Contains(req.Query, "milestones("):
			if owner != "rapira-rs" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"milestones":   map[string]any{"nodes": []map[string]any{}},
							"pullRequests": map[string]any{"nodes": []map[string]any{}},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"milestones": map[string]any{
							"nodes": []map[string]any{
								{
									"number":       1,
									"title":        "v1.0",
									"description":  "Transport unification",
									"state":        "OPEN",
									"dueOn":        "2027-01-15T00:00:00Z",
									"updatedAt":    recentOpen,
									"openIssues":   map[string]any{"totalCount": 5},
									"closedIssues": map[string]any{"totalCount": 12},
								},
							},
						},
						"pullRequests": map[string]any{
							"nodes": []map[string]any{
								{
									"number":    401,
									"state":     "OPEN",
									"title":     "feat: draft broker trait",
									"body":      "Draft PR for future AMQP and Kafka brokers.",
									"isDraft":   true,
									"url":       "https://github.com/rapira-rs/rapira/pull/401",
									"createdAt": recentOpen,
									"updatedAt": recentOpen,
									"labels":    map[string]any{"nodes": []map[string]any{{"name": "enhancement"}}},
									"milestone": map[string]any{"title": "v1.0"},
								},
								{
									"number":    402,
									"state":     "MERGED",
									"title":     "feat: pool buffer reuse",
									"body":      "Merged PR that reduced frame allocations.",
									"isDraft":   false,
									"url":       "https://github.com/rapira-rs/rapira/pull/402",
									"createdAt": recentClosed,
									"updatedAt": recentClosed,
									"labels":    map[string]any{"nodes": []map[string]any{{"name": "performance"}}},
								},
								{
									"number":    403,
									"state":     "CLOSED",
									"title":     "old closed PR outside lookback",
									"body":      "Should be skipped by 90d cutoff.",
									"isDraft":   false,
									"url":       "https://github.com/rapira-rs/rapira/pull/403",
									"createdAt": ancientClosed,
									"updatedAt": ancientClosed,
									"labels":    map[string]any{"nodes": []map[string]any{}},
								},
							},
						},
					},
				},
			})

		case strings.Contains(req.Query, "issues(states: [OPEN]"):
			if owner != "rapira-rs" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"issues": map[string]any{
								"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
								"nodes":    []map[string]any{},
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
							"nodes": []map[string]any{
								{
									"number":           37,
									"state":            "OPEN",
									"title":            "Unified broker and stream interface",
									"body":             "Support future brokers; test secret " + secretTok,
									"url":              "https://github.com/rapira-rs/rapira/issues/37",
									"createdAt":        recentOpen,
									"updatedAt":        recentOpen,
									"issueType":        map[string]any{"name": "Feature"},
									"labels":           map[string]any{"nodes": []map[string]any{{"name": "architecture"}}},
									"milestone":        map[string]any{"title": "v1.0"},
									"parent":           map[string]any{"number": 33},
									"subIssuesSummary": map[string]any{"total": 8, "completed": 5},
								},
							},
						},
					},
				},
			})

		case strings.Contains(req.Query, "issues(states: [CLOSED]"):
			if owner != "rapira-rs" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"issues": map[string]any{"nodes": []map[string]any{}},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{
							"nodes": []map[string]any{
								{
									"number":           30,
									"state":            "CLOSED",
									"title":            "Recently shipped ZTS worker pool",
									"body":             "Implemented single-process ZTS worker pool.",
									"url":              "https://github.com/rapira-rs/rapira/issues/30",
									"createdAt":        recentClosed,
									"updatedAt":        recentClosed,
									"labels":           map[string]any{"nodes": []map[string]any{}},
									"subIssuesSummary": map[string]any{"total": 0, "completed": 0},
								},
								{
									"number":           5,
									"state":            "CLOSED",
									"title":            "Ancient issue outside lookback",
									"body":             "Should be skipped by 90d cutoff.",
									"url":              "https://github.com/rapira-rs/rapira/issues/5",
									"createdAt":        ancientClosed,
									"updatedAt":        ancientClosed,
									"labels":           map[string]any{"nodes": []map[string]any{}},
									"subIssuesSummary": map[string]any{"total": 0, "completed": 0},
								},
							},
						},
					},
				},
			})

		case strings.Contains(req.Query, "discussions("):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"discussions": map[string]any{
							"nodes": []map[string]any{
								{
									"number":    12,
									"title":     "RFC: Streaming RPC and Queue Architecture",
									"body":      "Proposal for unifying streaming and queue consumers.",
									"url":       "https://github.com/rapira-rs/rapira/discussions/12",
									"closed":    false,
									"createdAt": recentOpen,
									"updatedAt": recentOpen,
									"category":  map[string]any{"name": "Ideas"},
									"labels":    map[string]any{"nodes": []map[string]any{{"name": "rfc"}}},
								},
							},
						},
					},
				},
			})
		}
	}))
	defer gqlSrv.Close()

	st := newFakeSyncStore()
	st.projByRepo["rapira-rs/rapira"] = []string{"core", "rapira"}
	st.repos["rapira-rs/newly-archived"] = store.GitHubRepo{
		FullName:   "rapira-rs/newly-archived",
		Owner:      "rapira-rs",
		Name:       "newly-archived",
		Project:    "newly-archived",
		IsArchived: false,
	}

	syncer := githubsync.New(githubsync.Config{
		Store:        st,
		Owners:       []string{"rapira-rs", "rustatian"},
		DefaultToken: "tok-default",
		OwnerTokens:  map[string]string{"rapira-rs": "tok-rapira"},
		GraphQLURL:   gqlSrv.URL,
		Now:          func() time.Time { return now },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	sum1, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll #1: %v", err)
	}
	if sum1.Owners != 2 || sum1.ReposScanned != 2 || len(sum1.Errors) != 0 {
		t.Fatalf("unexpected summary #1: %+v", sum1)
	}
	if seenAuth["rapira-rs"] != "Bearer tok-rapira" || seenAuth["rustatian"] != "Bearer tok-default" {
		t.Fatalf("per-owner token selection failed: %+v", seenAuth)
	}
	// Verify untracked archived repo is not inserted, tracked archived repo is updated, ancient closed items are skipped, and 5 items are synced.
	if _, exists := st.repos["rapira-rs/old-archived"]; exists {
		t.Fatalf("untracked archived repo should not be inserted")
	}
	if !st.repos["rapira-rs/newly-archived"].IsArchived {
		t.Fatalf("previously tracked archived repo should be marked archived")
	}
	if _, exists := st.items["rapira-rs/rapira#issue:5"]; exists {
		t.Fatalf("ancient closed issue #5 should have been skipped")
	}
	if _, exists := st.items["rapira-rs/rapira#pr:403"]; exists {
		t.Fatalf("ancient closed PR #403 should have been skipped")
	}
	for _, wantID := range []string{
		"rapira-rs/rapira#issue:37",
		"rapira-rs/rapira#issue:30",
		"rapira-rs/rapira#pr:401",
		"rapira-rs/rapira#pr:402",
		"rapira-rs/rapira#discussion:12",
	} {
		if _, ok := st.items[wantID]; !ok {
			t.Fatalf("missing synced item %q; have %v", wantID, st.items)
		}
	}
	issue37 := st.items["rapira-rs/rapira#issue:37"]
	if findings := secrets.Scan(issue37.Body); len(findings) != 0 {
		t.Fatalf("secret leaked into github item body: %+v", findings)
	}
	if issue37.ParentNumber == nil || *issue37.ParentNumber != 33 || issue37.SubIssuesTotal != 8 {
		t.Fatalf("parent/sub-issues not preserved: %+v", issue37)
	}
	if st.items["rapira-rs/rapira#pr:401"].State != "draft" {
		t.Fatalf("draft PR state = %q, want draft", st.items["rapira-rs/rapira#pr:401"].State)
	}
	if st.items["rapira-rs/rapira#pr:402"].State != "merged" {
		t.Fatalf("merged PR state = %q, want merged", st.items["rapira-rs/rapira#pr:402"].State)
	}
	// Verify org roadmap job is enqueued before mapped project roadmap jobs.
	wantJobs := []string{
		"distill_org_roadmap:org:rapira-rs",
		"distill_project_roadmap:project:core",
		"distill_project_roadmap:project:rapira",
	}
	if !slices.Equal(st.jobs, wantJobs) {
		t.Fatalf("enqueued jobs = %v, want %v", st.jobs, wantJobs)
	}

	// Second identical sync must enqueue zero new roadmap jobs.
	st.jobs = nil
	sum2, err := syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll #2: %v", err)
	}
	if len(st.jobs) != 0 || sum2.ItemsChanged != 0 {
		t.Fatalf("second identical sync not idempotent: jobs=%v summary=%+v", st.jobs, sum2)
	}
}
