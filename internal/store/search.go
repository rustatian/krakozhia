package store

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"
)

// Query forms supported by SearchKnowledge.
const (
	FormText   = "text"
	FormVector = "vector"
	FormHybrid = "hybrid"
)

const rrfK = 60.0

// SearchQuery configures hybrid retrieval across document chunks and memory notes.
type SearchQuery struct {
	Form               string
	Text               string
	QueryVector        []float32
	Kind               string // "", "doc", or "note"
	Project            string
	ExcludeNoteProject string
	Type               string
	Tag                string
	Limit              int
	DedupeDocs         bool
}

// SearchHit is one ranked result from SearchKnowledge.
type SearchHit struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	DocumentID  string    `json:"document_id,omitempty"`
	Project     string    `json:"project"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	HeadingPath string    `json:"heading_path,omitempty"`
	Description string    `json:"description,omitempty"`
	Snippet     string    `json:"snippet"`
	UpdatedAt   time.Time `json:"updated_at"`
	Distance    *float64  `json:"distance,omitempty"`
	TextScore   *float64  `json:"text_score,omitempty"`
	Score       float64   `json:"score"`
}

// ComputeRRF combines 1-based vector and full-text ranks with reciprocal rank fusion (k=60).
func ComputeRRF(vecRank, textRank int) float64 {
	var score float64
	if vecRank > 0 {
		score += 1.0 / (rrfK + float64(vecRank))
	}
	if textRank > 0 {
		score += 1.0 / (rrfK + float64(textRank))
	}
	return score
}

// SearchKnowledge runs vector, full-text, or hybrid RRF search across chunks and notes.
func (s *Store) SearchKnowledge(ctx context.Context, q SearchQuery) ([]SearchHit, error) {
	form := q.Form
	if form == "" {
		form = FormHybrid
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	poolLimit := max(limit*4, 40)

	type candidate struct {
		hit      SearchHit
		vecRank  int
		textRank int
	}
	byKey := map[string]*candidate{}
	upsert := func(h SearchHit) *candidate {
		key := h.Kind + ":" + h.ID
		if c, ok := byKey[key]; ok {
			return c
		}
		c := &candidate{hit: h}
		byKey[key] = c
		return c
	}

	if form != FormText && len(q.QueryVector) > 0 {
		vecHits, err := s.searchVector(ctx, q, poolLimit)
		if err != nil {
			return nil, err
		}
		for i, h := range vecHits {
			c := upsert(h)
			c.vecRank = i + 1
			c.hit.Distance = h.Distance
		}
	}

	if form != FormVector && strings.TrimSpace(q.Text) != "" {
		textHits, err := s.searchFTS(ctx, q, poolLimit)
		if err != nil {
			return nil, err
		}
		for i, h := range textHits {
			c := upsert(h)
			c.textRank = i + 1
			c.hit.TextScore = h.TextScore
		}
	}

	merged := make([]SearchHit, 0, len(byKey))
	for _, c := range byKey {
		c.hit.Score = ComputeRRF(c.vecRank, c.textRank)
		merged = append(merged, c.hit)
	}
	slices.SortFunc(merged, func(a, b SearchHit) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		if a.Distance != nil && b.Distance != nil {
			if *a.Distance < *b.Distance {
				return -1
			}
			if *a.Distance > *b.Distance {
				return 1
			}
		}
		return strings.Compare(a.ID, b.ID)
	})

	if q.DedupeDocs {
		seenDocs := map[string]bool{}
		filtered := merged[:0]
		for _, h := range merged {
			if h.Kind == "doc" {
				if seenDocs[h.DocumentID] {
					continue
				}
				seenDocs[h.DocumentID] = true
			}
			filtered = append(filtered, h)
		}
		merged = filtered
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (s *Store) searchVector(ctx context.Context, q SearchQuery, limit int) ([]SearchHit, error) {
	vec := pgvector.NewVector(q.QueryVector)
	rows, err := s.pool.Query(ctx, `
		SELECT kind, id, document_id, project, item_type, title, heading_path, description, snippet, updated_at, distance
		FROM (
			SELECT 'doc'::text AS kind, c.id, c.document_id, d.project, d.doc_type AS item_type,
			       d.title, c.heading_path, d.summary AS description, c.content AS snippet,
			       d.updated_at, (c.embedding <=> $1) AS distance
			FROM chunks c
			JOIN documents d ON d.id = c.document_id
			WHERE ($2 = '' OR $2 = 'doc')
			  AND ($3 = '' OR d.project = $3)
			  AND ($5 = '' OR d.doc_type = $5)
			  AND ($6 = '' OR $6 = ANY(d.tags))
			  AND c.embedding IS NOT NULL
			UNION ALL
			SELECT 'note'::text AS kind, n.id, ''::text AS document_id, n.project, n.note_type AS item_type,
			       n.name AS title, ''::text AS heading_path, n.description, n.content AS snippet,
			       n.updated_at, (n.embedding <=> $1) AS distance
			FROM notes n
			WHERE ($2 = '' OR $2 = 'note')
			  AND ($3 = '' OR n.project = $3)
			  AND ($4 = '' OR n.project <> $4)
			  AND ($5 = '' OR n.note_type = $5)
			  AND ($6 = '' OR $6 = ANY(n.tags))
			  AND n.embedding IS NOT NULL
		) AS candidates
		ORDER BY distance ASC, id ASC
		LIMIT $7`, vec, q.Kind, q.Project, q.ExcludeNoteProject, q.Type, q.Tag, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var dist float64
		if err := rows.Scan(
			&h.Kind, &h.ID, &h.DocumentID, &h.Project, &h.Type,
			&h.Title, &h.HeadingPath, &h.Description, &h.Snippet, &h.UpdatedAt, &dist); err != nil {
			return nil, err
		}
		h.Distance = &dist
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) searchFTS(ctx context.Context, q SearchQuery, limit int) ([]SearchHit, error) {
	rows, err := s.pool.Query(ctx, `
		WITH queries AS (
			-- Build OR-combined tsquery lexemes so long prompts match partial terms without ANDing every word.
			SELECT
				coalesce(
					(SELECT string_agg('''' || replace(replace(w, '\', '\\'), '''', '''''') || '''', ' | ')::tsquery
					 FROM unnest(tsvector_to_array(to_tsvector('english', $1))) AS w),
					''::tsquery
				) AS q_en,
				coalesce(
					(SELECT string_agg('''' || replace(replace(w, '\', '\\'), '''', '''''') || '''', ' | ')::tsquery
					 FROM unnest(tsvector_to_array(to_tsvector('simple', $1))) AS w
					 WHERE to_tsvector('english', w) <> ''::tsvector
					   AND length(w) >= 2),
					''::tsquery
				) AS q_simple
		)
		SELECT kind, id, document_id, project, item_type, title, heading_path, description, snippet, updated_at, score
		FROM (
			SELECT 'doc'::text AS kind, c.id, c.document_id, d.project, d.doc_type AS item_type,
			       d.title, c.heading_path, d.summary AS description, c.content AS snippet,
			       d.updated_at,
			       (ts_rank_cd(c.tsv_en, queries.q_en, 1 | 32) + ts_rank_cd(c.tsv_simple, queries.q_simple, 1 | 32))::double precision AS score
			FROM chunks c
			JOIN documents d ON d.id = c.document_id
			CROSS JOIN queries
			WHERE ($2 = '' OR $2 = 'doc')
			  AND ($3 = '' OR d.project = $3)
			  AND ($5 = '' OR d.doc_type = $5)
			  AND ($6 = '' OR $6 = ANY(d.tags))
			  AND (c.tsv_en @@ queries.q_en OR c.tsv_simple @@ queries.q_simple)
			UNION ALL
			SELECT 'note'::text AS kind, n.id, ''::text AS document_id, n.project, n.note_type AS item_type,
			       n.name AS title, ''::text AS heading_path, n.description, n.content AS snippet,
			       n.updated_at,
			       (ts_rank_cd(n.tsv_en, queries.q_en, 1 | 32) + ts_rank_cd(n.tsv_simple, queries.q_simple, 1 | 32))::double precision AS score
			FROM notes n
			CROSS JOIN queries
			WHERE ($2 = '' OR $2 = 'note')
			  AND ($3 = '' OR n.project = $3)
			  AND ($4 = '' OR n.project <> $4)
			  AND ($5 = '' OR n.note_type = $5)
			  AND ($6 = '' OR $6 = ANY(n.tags))
			  AND (n.tsv_en @@ queries.q_en OR n.tsv_simple @@ queries.q_simple)
		) AS candidates
		ORDER BY score DESC, id ASC
		LIMIT $7`, q.Text, q.Kind, q.Project, q.ExcludeNoteProject, q.Type, q.Tag, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var score float64
		if err := rows.Scan(
			&h.Kind, &h.ID, &h.DocumentID, &h.Project, &h.Type,
			&h.Title, &h.HeadingPath, &h.Description, &h.Snippet, &h.UpdatedAt, &score); err != nil {
			return nil, err
		}
		h.TextScore = &score
		out = append(out, h)
	}
	return out, rows.Err()
}
