package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

type evalPrompt struct {
	Prompt   string   `json:"prompt"`
	Project  string   `json:"project"`
	Relevant []string `json:"relevant"`
}

type evalSummary struct {
	Form            string
	RecallAtK       float64
	MRR             float64
	FalseInjections int
	NegativeCount   int
	FalseInjectRate float64
	P50Ms           float64
	P95Ms           float64
	PosDistP50      string
	PosDistP90      string
	NegDistMin      string
	PosTextP50      string
	NegTextMax      string
}

func runEval(args []string, stdout io.Writer) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rerank := fs.Bool("rerank", false, "also evaluate hybrid+rerank")
	kind := fs.String("kind", "note", "kind filter (note, doc, github, or empty for all)")
	maxDist := fs.Float64("max-distance", cfg.MaxDistance, "gate distance limit")
	minText := fs.Float64("min-text-score", cfg.MinTextScore, "gate full-text score floor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: krakozhia eval [--rerank] [--kind note|doc|github|\"\"] [--max-distance F] [--min-text-score F] <testset.jsonl>")
	}

	prompts, err := loadTestset(fs.Arg(0))
	if err != nil {
		return err
	}
	forms := []string{store.FormText, store.FormVector, store.FormHybrid}
	if *rerank {
		forms = append(forms, "hybrid+rerank")
	}

	client := NewClient(cfg.URL, cfg.Token)
	fmt.Fprintf(stdout, "Evaluating %d prompts (k=%d max_distance=%.3f min_text_score=%.3f kind=%q)\n",
		len(prompts), cfg.MaxPointers, *maxDist, *minText, *kind)
	fmt.Fprintf(stdout, "%-14s  recall@%-2d  mrr    false_inj        p50_ms  p95_ms  pos_d50  pos_d90  neg_dmin  pos_t50  neg_tmax\n",
		"form", cfg.MaxPointers)

	for _, form := range forms {
		s, err := evalForm(context.Background(), client, prompts, form, *kind, *maxDist, *minText, cfg.MaxPointers)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%-14s  %8.3f  %.3f  %2d/%-2d (%5.1f%%)  %6.1f  %6.1f  %7s  %7s  %8s  %7s  %8s\n",
			s.Form, s.RecallAtK, s.MRR, s.FalseInjections, s.NegativeCount, s.FalseInjectRate*100.0,
			s.P50Ms, s.P95Ms, s.PosDistP50, s.PosDistP90, s.NegDistMin, s.PosTextP50, s.NegTextMax)
	}
	return nil
}

func loadTestset(path string) ([]evalPrompt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var prompts []evalPrompt
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p evalPrompt
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse testset: %w", err)
		}
		prompts = append(prompts, p)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(prompts) == 0 {
		return nil, fmt.Errorf("testset %s is empty", path)
	}
	return prompts, nil
}

func evalForm(ctx context.Context, client *Client, prompts []evalPrompt, form, kind string, maxDist, minText float64, k int) (evalSummary, error) {
	apiForm := form
	useRerank := false
	if form == "hybrid+rerank" {
		apiForm = store.FormHybrid
		useRerank = true
	}

	var (
		latencies []float64
		posDists  []float64
		negDists  []float64
		posTexts  []float64
		negTexts  []float64
		sumRecall float64
		sumMRR    float64
		posCount  int
		negCount  int
		falseInj  int
	)

	for _, p := range prompts {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		start := time.Now()
		resp, err := client.Search(reqCtx, server.SearchRequest{
			Form:               apiForm,
			Query:              truncateRunes(strings.TrimSpace(p.Prompt), maxQueryRunes),
			Kind:               kind,
			ExcludeNoteProject: p.Project,
			Limit:              recallCandidateLim,
			DedupeDocs:         true,
			Rerank:             useRerank,
		})
		elapsed := float64(time.Since(start).Microseconds()) / 1000.0
		cancel()
		if err != nil {
			return evalSummary{}, fmt.Errorf("%s search: %w", form, err)
		}
		latencies = append(latencies, elapsed)

		var gated []store.SearchHit
		for _, h := range resp.Hits {
			if RecallGate(h, maxDist, minText) {
				gated = append(gated, h)
			}
			if len(gated) >= k {
				break
			}
		}

		if len(p.Relevant) == 0 {
			negCount++
			if len(gated) > 0 {
				falseInj++
			}
			if len(resp.Hits) > 0 {
				if resp.Hits[0].Distance != nil {
					negDists = append(negDists, *resp.Hits[0].Distance)
				}
				if resp.Hits[0].TextScore != nil {
					negTexts = append(negTexts, *resp.Hits[0].TextScore)
				}
			}
			continue
		}

		posCount++
		relSet := make(map[string]bool, len(p.Relevant))
		for _, id := range p.Relevant {
			relSet[id] = true
		}
		matched := map[string]bool{}
		firstRank := 0
		for i, h := range gated {
			if id, ok := matchRelevant(h, relSet); ok {
				matched[id] = true
				if firstRank == 0 {
					firstRank = i + 1
				}
			}
		}
		sumRecall += float64(len(matched)) / float64(len(relSet))
		if firstRank > 0 {
			sumMRR += 1.0 / float64(firstRank)
		}

		for _, h := range resp.Hits {
			if _, ok := matchRelevant(h, relSet); ok {
				if h.Distance != nil {
					posDists = append(posDists, *h.Distance)
				}
				if h.TextScore != nil {
					posTexts = append(posTexts, *h.TextScore)
				}
				break
			}
		}
	}

	s := evalSummary{
		Form:            form,
		FalseInjections: falseInj,
		NegativeCount:   negCount,
		P50Ms:           percentile(latencies, 0.50),
		P95Ms:           percentile(latencies, 0.95),
		PosDistP50:      fmtOptPct(posDists, 0.50),
		PosDistP90:      fmtOptPct(posDists, 0.90),
		NegDistMin:      fmtOptPct(negDists, 0.00),
		PosTextP50:      fmtOptPct(posTexts, 0.50),
		NegTextMax:      fmtOptPct(negTexts, 1.00),
	}
	if posCount > 0 {
		s.RecallAtK = sumRecall / float64(posCount)
		s.MRR = sumMRR / float64(posCount)
	}
	if negCount > 0 {
		s.FalseInjectRate = float64(falseInj) / float64(negCount)
	}
	return s, nil
}

func matchRelevant(h store.SearchHit, rel map[string]bool) (string, bool) {
	if rel[h.ID] {
		return h.ID, true
	}
	if h.Kind == "doc" && h.DocumentID != "" && rel[h.DocumentID] {
		return h.DocumentID, true
	}
	return "", false
}

func percentile(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := slices.Clone(vals)
	slices.Sort(cp)
	if q <= 0 {
		return cp[0]
	}
	if q >= 1 {
		return cp[len(cp)-1]
	}
	idx := int(q*float64(len(cp)-1) + 0.5)
	return cp[idx]
}

func fmtOptPct(vals []float64, q float64) string {
	if len(vals) == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", percentile(vals, q))
}
