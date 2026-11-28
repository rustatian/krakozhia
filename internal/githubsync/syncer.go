// Package githubsync incrementally synchronizes GitHub repositories, milestones, issues, pull requests, and discussions.
package githubsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rustatian/krakozhia/internal/secrets"
	"github.com/rustatian/krakozhia/internal/store"
)

const (
	defaultGraphQLURL     = "https://api.github.com/graphql"
	defaultSyncInterval   = 2 * time.Hour
	defaultClosedLookback = 90 * 24 * time.Hour
	maxItemBodyRunes      = 6000
	startupSyncDelay      = 15 * time.Second
)

// Store abstracts persistence methods required by Syncer.
type Store interface {
	GetGitHubRepo(ctx context.Context, fullName string) (store.GitHubRepo, error)
	UpsertGitHubRepo(ctx context.Context, repo store.GitHubRepo) error
	UpsertGitHubMilestones(ctx context.Context, repoFullName string, milestones []store.GitHubMilestone) error
	UpsertGitHubItem(ctx context.Context, item store.GitHubItem) (bool, error)
	ListProjectsForRepo(ctx context.Context, repoFullName string) ([]string, error)
	EnqueueJob(ctx context.Context, jobType, targetKind, targetID string) error
}

// Config configures a GitHub Syncer.
type Config struct {
	Store          Store
	Owners         []string
	DefaultToken   string
	OwnerTokens    map[string]string
	GraphQLURL     string
	SyncInterval   time.Duration
	ClosedLookback time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
	Logger         *slog.Logger
}

// SyncSummary reports totals from one SyncAll pass.
type SyncSummary struct {
	Owners        int      `json:"owners"`
	ReposScanned  int      `json:"repos_scanned"`
	ItemsUpserted int      `json:"items_upserted"`
	ItemsChanged  int      `json:"items_changed"`
	Errors        []string `json:"errors,omitempty"`
}

// Syncer polls allowlisted GitHub owners and updates PostgreSQL roadmap tables.
type Syncer struct {
	store          Store
	owners         []string
	defaultToken   string
	ownerTokens    map[string]string
	graphqlURL     string
	syncInterval   time.Duration
	closedLookback time.Duration
	httpClient     *http.Client
	now            func() time.Time
	log            *slog.Logger
	mu             sync.Mutex
}

// New creates a configured Syncer.
func New(cfg Config) *Syncer {
	gqlURL := strings.TrimSpace(cfg.GraphQLURL)
	if gqlURL == "" {
		gqlURL = defaultGraphQLURL
	}
	interval := cfg.SyncInterval
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	lookback := cfg.ClosedLookback
	if lookback <= 0 {
		lookback = defaultClosedLookback
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tokens := make(map[string]string, len(cfg.OwnerTokens))
	for k, v := range cfg.OwnerTokens {
		if lk := strings.ToLower(strings.TrimSpace(k)); lk != "" && strings.TrimSpace(v) != "" {
			tokens[lk] = strings.TrimSpace(v)
		}
	}
	return &Syncer{
		store:          cfg.Store,
		owners:         cfg.Owners,
		defaultToken:   strings.TrimSpace(cfg.DefaultToken),
		ownerTokens:    tokens,
		graphqlURL:     gqlURL,
		syncInterval:   interval,
		closedLookback: lookback,
		httpClient:     httpClient,
		now:            nowFn,
		log:            logger,
	}
}

// ParseOwners splits a comma-separated owner allowlist into deduplicated owner names.
func ParseOwners(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	return out
}

// ParseOwnerTokens parses comma- or semicolon-separated "owner=token" overrides.
func ParseOwnerTokens(raw string) map[string]string {
	out := map[string]string{}
	replacer := strings.NewReplacer(";", ",")
	for _, part := range strings.Split(replacer.Replace(raw), ",") {
		owner, tok, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		owner = strings.ToLower(strings.TrimSpace(owner))
		tok = strings.TrimSpace(tok)
		if owner != "" && tok != "" {
			out[owner] = tok
		}
	}
	return out
}

// Run executes an initial delayed sync and then repeats on SyncInterval until ctx is canceled.
func (s *Syncer) Run(ctx context.Context) {
	if len(s.owners) == 0 {
		return
	}
	delay := time.NewTimer(startupSyncDelay)
	select {
	case <-ctx.Done():
		delay.Stop()
		return
	case <-delay.C:
	}

	s.runOnceLogged(ctx)
	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnceLogged(ctx)
		}
	}
}

func (s *Syncer) runOnceLogged(ctx context.Context) {
	sum, err := s.SyncAll(ctx)
	if err != nil {
		s.log.Warn("github sync failed", "err", err)
		return
	}
	s.log.Info("github sync complete",
		"owners", sum.Owners,
		"repos", sum.ReposScanned,
		"items_upserted", sum.ItemsUpserted,
		"items_changed", sum.ItemsChanged,
		"errors", len(sum.Errors),
	)
}

// SyncAll synchronizes every allowlisted owner sequentially and queues roadmap jobs for changed repos and orgs.
func (s *Syncer) SyncAll(ctx context.Context) (SyncSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sum SyncSummary
	for _, owner := range s.owners {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		tok := s.tokenForOwner(owner)
		if tok == "" {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: no GitHub token configured", owner))
			continue
		}
		sum.Owners++
		ownerSum, changedProjects, err := s.syncOwner(ctx, owner, tok)
		sum.ReposScanned += ownerSum.ReposScanned
		sum.ItemsUpserted += ownerSum.ItemsUpserted
		sum.ItemsChanged += ownerSum.ItemsChanged
		sum.Errors = append(sum.Errors, ownerSum.Errors...)
		if err != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: %v", owner, err))
			continue
		}
		if len(changedProjects) > 0 {
			// Enqueue the org roadmap before project roadmaps so project synthesis sees the updated org direction.
			if err := s.store.EnqueueJob(ctx, "distill_org_roadmap", "org", owner); err != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s enqueue org roadmap: %v", owner, err))
			}
			for _, proj := range changedProjects {
				if err := s.store.EnqueueJob(ctx, "distill_project_roadmap", "project", proj); err != nil {
					sum.Errors = append(sum.Errors, fmt.Sprintf("%s enqueue roadmap %s: %v", owner, proj, err))
				}
			}
		}
	}
	return sum, nil
}

func (s *Syncer) tokenForOwner(owner string) string {
	if tok, ok := s.ownerTokens[strings.ToLower(strings.TrimSpace(owner))]; ok && tok != "" {
		return tok
	}
	return s.defaultToken
}

type gqlRepoNode struct {
	Name                  string `json:"name"`
	NameWithOwner         string `json:"nameWithOwner"`
	Description           string `json:"description"`
	IsPrivate             bool   `json:"isPrivate"`
	IsArchived            bool   `json:"isArchived"`
	HasDiscussionsEnabled bool   `json:"hasDiscussionsEnabled"`
	PushedAt              string `json:"pushedAt"`
	Issues                struct {
		TotalCount int `json:"totalCount"`
	} `json:"issues"`
	PullRequests struct {
		TotalCount int `json:"totalCount"`
	} `json:"pullRequests"`
}

func (s *Syncer) syncOwner(ctx context.Context, owner, token string) (SyncSummary, []string, error) {
	repos, err := s.listOwnerRepos(ctx, owner, token)
	if err != nil {
		return SyncSummary{}, nil, err
	}

	var sum SyncSummary
	var changedProjects []string
	seenProj := map[string]bool{}
	cutoff := s.now().Add(-s.closedLookback)

	for _, r := range repos {
		if ctx.Err() != nil {
			return sum, changedProjects, ctx.Err()
		}
		pushedAt := parseOptionalTime(r.PushedAt)
		if r.IsArchived {
			if existing, err := s.store.GetGitHubRepo(ctx, r.NameWithOwner); err == nil && !existing.IsArchived {
				existing.IsArchived = true
				_ = s.store.UpsertGitHubRepo(ctx, existing)
			}
			continue
		}

		sum.ReposScanned++
		existing, getErr := s.store.GetGitHubRepo(ctx, r.NameWithOwner)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: load repo: %v", r.NameWithOwner, getErr))
			continue
		}

		now := s.now()
		repoRecord := store.GitHubRepo{
			FullName:       r.NameWithOwner,
			Owner:          owner,
			Name:           r.Name,
			Project:        r.Name,
			Description:    strings.TrimSpace(r.Description),
			IsPrivate:      r.IsPrivate,
			IsArchived:     false,
			HasDiscussions: r.HasDiscussionsEnabled,
			PushedAt:       pushedAt,
			LastSyncedAt:   &now,
		}
		// Ensure newly discovered repos exist in github_repos before child milestone/item foreign keys.
		if errors.Is(getErr, store.ErrNotFound) {
			if err := s.store.UpsertGitHubRepo(ctx, repoRecord); err != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: upsert repo: %v", r.NameWithOwner, err))
				continue
			}
		}

		// Skip GraphQL item fetches for dormant repos with zero open issues/PRs and no prior synced items.
		dormant := r.Issues.TotalCount == 0 && r.PullRequests.TotalCount == 0 && !r.HasDiscussionsEnabled &&
			existing.SyncHash == "" && (pushedAt == nil || pushedAt.Before(cutoff))
		if dormant {
			if !errors.Is(getErr, store.ErrNotFound) {
				_ = s.store.UpsertGitHubRepo(ctx, repoRecord)
			}
			continue
		}

		milestones, items, warnErrs := s.fetchRepoData(ctx, owner, r.Name, r.NameWithOwner, r.HasDiscussionsEnabled, cutoff, token)
		sum.Errors = append(sum.Errors, warnErrs...)

		syncHash := computeRepoSyncHash(r.Description, milestones, items)
		repoRecord.SyncHash = syncHash
		if err := s.store.UpsertGitHubRepo(ctx, repoRecord); err != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: save sync hash: %v", r.NameWithOwner, err))
		}
		if !errors.Is(getErr, store.ErrNotFound) && existing.SyncHash == syncHash {
			continue
		}

		if err := s.store.UpsertGitHubMilestones(ctx, r.NameWithOwner, milestones); err != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("%s: milestones: %v", r.NameWithOwner, err))
		}
		for i := range items {
			sum.ItemsUpserted++
			changed, err := s.store.UpsertGitHubItem(ctx, items[i])
			if err != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: upsert %s: %v", r.NameWithOwner, items[i].ID, err))
				continue
			}
			if changed {
				sum.ItemsChanged++
			}
		}

		if len(items) > 0 || len(milestones) > 0 {
			projects, err := s.store.ListProjectsForRepo(ctx, r.NameWithOwner)
			if err != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: list projects: %v", r.NameWithOwner, err))
				continue
			}
			for _, proj := range projects {
				if !seenProj[proj] {
					seenProj[proj] = true
					changedProjects = append(changedProjects, proj)
				}
			}
		}
	}
	return sum, changedProjects, nil
}

const listOwnerReposQuery = `
query($owner: String!, $cursor: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 100, after: $cursor, ownerAffiliations: [OWNER], orderBy: {field: PUSHED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        nameWithOwner
        description
        isPrivate
        isArchived
        hasDiscussionsEnabled
        pushedAt
        issues(states: [OPEN]) { totalCount }
        pullRequests(states: [OPEN]) { totalCount }
      }
    }
  }
}`

func (s *Syncer) listOwnerRepos(ctx context.Context, owner, token string) ([]gqlRepoNode, error) {
	var all []gqlRepoNode
	var cursor any
	for page := 0; page < 20; page++ {
		vars := map[string]any{"owner": owner, "cursor": cursor}
		var resp struct {
			RepositoryOwner *struct {
				Repositories struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []gqlRepoNode `json:"nodes"`
				} `json:"repositories"`
			} `json:"repositoryOwner"`
		}
		if err := s.doGraphQL(ctx, token, listOwnerReposQuery, vars, &resp); err != nil {
			return nil, err
		}
		if resp.RepositoryOwner == nil {
			return nil, fmt.Errorf("owner %q not found", owner)
		}
		all = append(all, resp.RepositoryOwner.Repositories.Nodes...)
		if !resp.RepositoryOwner.Repositories.PageInfo.HasNextPage || resp.RepositoryOwner.Repositories.PageInfo.EndCursor == "" {
			break
		}
		cursor = resp.RepositoryOwner.Repositories.PageInfo.EndCursor
	}
	return all, nil
}

const repoMilestonesAndPRsQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    milestones(first: 30, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        title
        description
        state
        dueOn
        updatedAt
        openIssues: issues(states: [OPEN]) { totalCount }
        closedIssues: issues(states: [CLOSED]) { totalCount }
      }
    }
    pullRequests(first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        state
        title
        body
        isDraft
        url
        createdAt
        updatedAt
        labels(first: 15) { nodes { name } }
        milestone { title }
      }
    }
  }
}`

const repoOpenIssuesQuery = `
query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issues(states: [OPEN], first: 50, after: $cursor, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        state
        title
        body
        url
        createdAt
        updatedAt
        issueType { name }
        labels(first: 15) { nodes { name } }
        milestone { title }
        parent { number }
        subIssuesSummary { total completed }
      }
    }
  }
}`

const repoClosedIssuesQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issues(states: [CLOSED], first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        state
        title
        body
        url
        createdAt
        updatedAt
        issueType { name }
        labels(first: 15) { nodes { name } }
        milestone { title }
        parent { number }
        subIssuesSummary { total completed }
      }
    }
  }
}`

const repoDiscussionsQuery = `
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    discussions(first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number
        title
        body
        url
        closed
        createdAt
        updatedAt
        category { name }
        labels(first: 10) { nodes { name } }
      }
    }
  }
}`

type gqlIssueNode struct {
	Number    int    `json:"number"`
	State     string `json:"state"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	IssueType *struct {
		Name string `json:"name"`
	} `json:"issueType"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Parent *struct {
		Number int `json:"number"`
	} `json:"parent"`
	SubIssuesSummary *struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
	} `json:"subIssuesSummary"`
}

func (s *Syncer) fetchRepoData(
	ctx context.Context,
	owner, name, fullName string,
	hasDiscussions bool,
	closedCutoff time.Time,
	token string,
) ([]store.GitHubMilestone, []store.GitHubItem, []string) {
	var (
		milestones []store.GitHubMilestone
		items      []store.GitHubItem
		errs       []string
	)

	// 1. Milestones and open/draft PRs.
	var mpResp struct {
		Repository *struct {
			Milestones struct {
				Nodes []struct {
					Number       int                      `json:"number"`
					Title        string                   `json:"title"`
					Description  string                   `json:"description"`
					State        string                   `json:"state"`
					DueOn        string                   `json:"dueOn"`
					UpdatedAt    string                   `json:"updatedAt"`
					OpenIssues   struct{ TotalCount int } `json:"openIssues"`
					ClosedIssues struct{ TotalCount int } `json:"closedIssues"`
				} `json:"nodes"`
			} `json:"milestones"`
			PullRequests struct {
				Nodes []struct {
					Number    int    `json:"number"`
					State     string `json:"state"`
					Title     string `json:"title"`
					Body      string `json:"body"`
					IsDraft   bool   `json:"isDraft"`
					URL       string `json:"url"`
					CreatedAt string `json:"createdAt"`
					UpdatedAt string `json:"updatedAt"`
					Labels    struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					Milestone *struct {
						Title string `json:"title"`
					} `json:"milestone"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := s.doGraphQL(ctx, token, repoMilestonesAndPRsQuery, map[string]any{"owner": owner, "name": name}, &mpResp); err != nil {
		errs = append(errs, fmt.Sprintf("%s milestones/prs: %v", fullName, err))
	} else if mpResp.Repository != nil {
		for _, m := range mpResp.Repository.Milestones.Nodes {
			desc, _ := secrets.Redact(truncateRunes(strings.TrimSpace(m.Description), 2000))
			milestones = append(milestones, store.GitHubMilestone{
				RepoFullName: fullName,
				Number:       m.Number,
				Title:        strings.TrimSpace(m.Title),
				Description:  desc,
				State:        strings.ToLower(m.State),
				DueOn:        parseOptionalTime(m.DueOn),
				OpenIssues:   m.OpenIssues.TotalCount,
				ClosedIssues: m.ClosedIssues.TotalCount,
				UpdatedAt:    parseTimeOrNow(m.UpdatedAt, s.now()),
			})
		}
		for _, pr := range mpResp.Repository.PullRequests.Nodes {
			updated := parseTimeOrNow(pr.UpdatedAt, s.now())
			state := strings.ToLower(strings.TrimSpace(pr.State))
			if state == "" || state == "open" {
				if pr.IsDraft {
					state = "draft"
				} else {
					state = "open"
				}
			} else if updated.Before(closedCutoff) {
				continue
			}
			var labels []string
			for _, l := range pr.Labels.Nodes {
				labels = append(labels, l.Name)
			}
			msTitle := ""
			if pr.Milestone != nil {
				msTitle = pr.Milestone.Title
			}
			items = append(items, sanitizeItem(store.GitHubItem{
				ID:              fmt.Sprintf("%s#pr:%d", fullName, pr.Number),
				RepoFullName:    fullName,
				Owner:           owner,
				Project:         name,
				Kind:            "pr",
				Number:          pr.Number,
				State:           state,
				Title:           pr.Title,
				Body:            pr.Body,
				Labels:          labels,
				MilestoneTitle:  msTitle,
				URL:             pr.URL,
				GitHubCreatedAt: parseTimeOrNow(pr.CreatedAt, s.now()),
				GitHubUpdatedAt: updated,
			}))
		}
	}

	// 2. Open issues (paginated completely).
	var cursor any
	for page := 0; page < 20; page++ {
		var openResp struct {
			Repository *struct {
				Issues struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []gqlIssueNode `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		if err := s.doGraphQL(ctx, token, repoOpenIssuesQuery, map[string]any{"owner": owner, "name": name, "cursor": cursor}, &openResp); err != nil {
			errs = append(errs, fmt.Sprintf("%s open issues: %v", fullName, err))
			break
		}
		if openResp.Repository == nil {
			break
		}
		for _, is := range openResp.Repository.Issues.Nodes {
			items = append(items, issueNodeToItem(owner, name, fullName, is, s.now()))
		}
		if !openResp.Repository.Issues.PageInfo.HasNextPage || openResp.Repository.Issues.PageInfo.EndCursor == "" {
			break
		}
		cursor = openResp.Repository.Issues.PageInfo.EndCursor
	}

	// 3. Recently closed issues within ClosedLookback (single recent page).
	var closedResp struct {
		Repository *struct {
			Issues struct {
				Nodes []gqlIssueNode `json:"nodes"`
			} `json:"issues"`
		} `json:"repository"`
	}
	if err := s.doGraphQL(ctx, token, repoClosedIssuesQuery, map[string]any{"owner": owner, "name": name}, &closedResp); err != nil {
		errs = append(errs, fmt.Sprintf("%s closed issues: %v", fullName, err))
	} else if closedResp.Repository != nil {
		for _, is := range closedResp.Repository.Issues.Nodes {
			updated := parseTimeOrNow(is.UpdatedAt, s.now())
			if updated.Before(closedCutoff) {
				continue
			}
			items = append(items, issueNodeToItem(owner, name, fullName, is, s.now()))
		}
	}

	// 4. Discussions when enabled on the repository.
	if hasDiscussions {
		var discResp struct {
			Repository *struct {
				Discussions struct {
					Nodes []struct {
						Number    int    `json:"number"`
						Title     string `json:"title"`
						Body      string `json:"body"`
						URL       string `json:"url"`
						Closed    bool   `json:"closed"`
						CreatedAt string `json:"createdAt"`
						UpdatedAt string `json:"updatedAt"`
						Category  *struct {
							Name string `json:"name"`
						} `json:"category"`
						Labels struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
					} `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		if err := s.doGraphQL(ctx, token, repoDiscussionsQuery, map[string]any{"owner": owner, "name": name}, &discResp); err != nil {
			// Discussions require an additional fine-grained token permission; log without failing issues/PRs.
			s.log.Warn("github discussions fetch skipped", "repo", fullName, "err", err)
		} else if discResp.Repository != nil {
			for _, d := range discResp.Repository.Discussions.Nodes {
				state := "open"
				if d.Closed {
					state = "closed"
				}
				cat := ""
				if d.Category != nil {
					cat = d.Category.Name
				}
				var labels []string
				for _, l := range d.Labels.Nodes {
					labels = append(labels, l.Name)
				}
				items = append(items, sanitizeItem(store.GitHubItem{
					ID:              fmt.Sprintf("%s#discussion:%d", fullName, d.Number),
					RepoFullName:    fullName,
					Owner:           owner,
					Project:         name,
					Kind:            "discussion",
					Number:          d.Number,
					State:           state,
					IssueType:       cat,
					Title:           d.Title,
					Body:            d.Body,
					Labels:          labels,
					URL:             d.URL,
					GitHubCreatedAt: parseTimeOrNow(d.CreatedAt, s.now()),
					GitHubUpdatedAt: parseTimeOrNow(d.UpdatedAt, s.now()),
				}))
			}
		}
	}

	return milestones, items, errs
}

func issueNodeToItem(owner, name, fullName string, is gqlIssueNode, now time.Time) store.GitHubItem {
	var labels []string
	for _, l := range is.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	issueType := ""
	if is.IssueType != nil {
		issueType = is.IssueType.Name
	}
	msTitle := ""
	if is.Milestone != nil {
		msTitle = is.Milestone.Title
	}
	var parentNum *int
	if is.Parent != nil && is.Parent.Number > 0 {
		n := is.Parent.Number
		parentNum = &n
	}
	subTotal, subCompleted := 0, 0
	if is.SubIssuesSummary != nil {
		subTotal = is.SubIssuesSummary.Total
		subCompleted = is.SubIssuesSummary.Completed
	}
	return sanitizeItem(store.GitHubItem{
		ID:                 fmt.Sprintf("%s#issue:%d", fullName, is.Number),
		RepoFullName:       fullName,
		Owner:              owner,
		Project:            name,
		Kind:               "issue",
		Number:             is.Number,
		State:              strings.ToLower(is.State),
		IssueType:          issueType,
		Title:              is.Title,
		Body:               is.Body,
		Labels:             labels,
		MilestoneTitle:     msTitle,
		ParentNumber:       parentNum,
		SubIssuesTotal:     subTotal,
		SubIssuesCompleted: subCompleted,
		URL:                is.URL,
		GitHubCreatedAt:    parseTimeOrNow(is.CreatedAt, now),
		GitHubUpdatedAt:    parseTimeOrNow(is.UpdatedAt, now),
	})
}

func sanitizeItem(it store.GitHubItem) store.GitHubItem {
	title, _ := secrets.Redact(strings.TrimSpace(it.Title))
	body, _ := secrets.Redact(truncateRunes(strings.TrimSpace(it.Body), maxItemBodyRunes))
	it.Title = title
	it.Body = body
	it.ContentHash = store.HashGitHubItem(it)
	return it
}

func computeRepoSyncHash(desc string, milestones []store.GitHubMilestone, items []store.GitHubItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "desc:%s\n", strings.TrimSpace(desc))
	msSorted := append([]store.GitHubMilestone(nil), milestones...)
	slices.SortFunc(msSorted, func(a, b store.GitHubMilestone) int { return a.Number - b.Number })
	for _, m := range msSorted {
		fmt.Fprintf(h, "m:%d:%s:%s:%d:%d\n", m.Number, m.State, m.Title, m.OpenIssues, m.ClosedIssues)
	}
	itSorted := append([]store.GitHubItem(nil), items...)
	slices.SortFunc(itSorted, func(a, b store.GitHubItem) int { return strings.Compare(a.ID, b.ID) })
	for _, it := range itSorted {
		fmt.Fprintf(h, "i:%s:%s\n", it.ID, it.ContentHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Syncer) doGraphQL(ctx context.Context, token, query string, vars map[string]any, dst any) error {
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": vars,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.graphqlURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "krakozhia/0.1")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github graphql status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode graphql envelope: %w", err)
	}
	if len(env.Errors) > 0 && len(env.Data) == 0 {
		return errors.New(env.Errors[0].Message)
	}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, dst); err != nil {
			return fmt.Errorf("decode graphql data: %w", err)
		}
	}
	if len(env.Errors) > 0 {
		return errors.New(env.Errors[0].Message)
	}
	return nil
}

func parseOptionalTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func parseTimeOrNow(raw string, fallback time.Time) time.Time {
	if t := parseOptionalTime(raw); t != nil {
		return *t
	}
	return fallback.UTC()
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
