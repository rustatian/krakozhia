package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	const ms = time.Millisecond
	// 20 ms down to 1 ms.
	var twenty []time.Duration
	for i := 20; i >= 1; i-- {
		twenty = append(twenty, time.Duration(i)*ms)
	}

	// The nearest-rank percentile is the value at rank ceil(p/100 * n) of the sorted values.
	tests := []struct {
		name   string
		values []time.Duration
		p      int
		want   time.Duration
	}{
		{name: "median of 20 values is the value of rank 10", values: twenty, p: 50, want: 10 * ms},
		{name: "95th percentile of 20 values is the value of rank 19", values: twenty, p: 95, want: 19 * ms},
		{name: "median of 3 values is the value of rank 2", values: []time.Duration{3 * ms, 1 * ms, 2 * ms}, p: 50, want: 2 * ms},
		{name: "95th percentile of 3 values is the largest value", values: []time.Duration{3 * ms, 1 * ms, 2 * ms}, p: 95, want: 3 * ms},
		{name: "one value", values: []time.Duration{7 * ms}, p: 50, want: 7 * ms},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.values, tt.p); got != tt.want {
				t.Errorf("percentile: got %v, want %v", got, tt.want)
			}
		})
	}
}

const evalTestSet = `{"prompt": "p1", "project": "proj-current", "relevant": ["proj-a/alpha"]}
{"prompt": "p2", "project": "proj-current", "relevant": ["proj-a/beta", "proj-b/gamma"]}
{"prompt": "p3", "project": "proj-current"}
{"prompt": "p4", "project": "proj-current", "relevant": []}
`

func TestEval(t *testing.T) {
	f, _ := newFake(t)
	recallHome(t) // the distance limit is 0.5 and the keyword floor is 8
	f.answers = map[string][]hit{
		// p1: alpha passes the floor, rank 1. p2: delta and beta pass, gamma does not; the first relevant note has rank 2.
		// p3: alpha passes, a false injection. p4: no note passes.
		"text:p1": {{ID: "proj-a/alpha", BM25: new(float32(9))}, {ID: "proj-a/beta", BM25: new(float32(3))}},
		"text:p2": {{ID: "proj-b/delta", BM25: new(float32(9))}, {ID: "proj-a/beta", BM25: new(float32(8.5))}, {ID: "proj-b/gamma", BM25: new(float32(2))}},
		"text:p3": {{ID: "proj-a/alpha", BM25: new(float32(9))}},
		"text:p4": {{ID: "proj-a/alpha", BM25: new(float32(2))}},
		// p1: alpha passes the limit, but it has rank 5 and only 3 notes are sent. p2: beta and gamma are sent.
		// p3 and p4: no note passes.
		"vector:p1": {{ID: "proj-a/beta", Distance: new(float32(0.3))}, {ID: "proj-b/delta", Distance: new(float32(0.4))}, {ID: "proj-b/gamma", Distance: new(float32(0.45))}, {ID: "proj-a/long", Distance: new(float32(0.46))}, {ID: "proj-a/alpha", Distance: new(float32(0.48))}},
		"vector:p2": {{ID: "proj-a/beta", Distance: new(float32(0.3))}, {ID: "proj-b/gamma", Distance: new(float32(0.4))}},
		"vector:p3": {{ID: "proj-a/alpha", Distance: new(float32(0.9))}},
		"vector:p4": {{ID: "proj-a/alpha", Distance: new(float32(0.8))}},
		// p1 and p2: each relevant note is sent, rank 1. p3: no note passes. p4: delta passes, a false injection.
		"hybrid:p1": {{ID: "proj-a/alpha", Distance: new(float32(0.4)), BM25: new(float32(9))}},
		"hybrid:p2": {{ID: "proj-a/beta", Distance: new(float32(0.4))}, {ID: "proj-b/gamma", Distance: new(float32(0.45))}},
		"hybrid:p3": {{ID: "proj-a/alpha", Distance: new(float32(0.9)), BM25: new(float32(2))}},
		"hybrid:p4": {{ID: "proj-b/delta", Distance: new(float32(0.45))}},
	}
	dir := t.TempDir()
	t.Chdir(dir)
	writeFiles(t, dir, map[string]string{"testset.jsonl": evalTestSet})
	var out bytes.Buffer

	if err := run([]string{"eval", "testset.jsonl"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("output: got %d lines, want a header and 3 rows:\n%s", len(lines), out.String())
	}
	if got, want := strings.Fields(lines[0]), []string{"form", "recall@3", "mrr", "false-injections", "p50-ms", "p95-ms"}; !slices.Equal(got, want) {
		t.Errorf("header: got %q, want %q", got, want)
	}
	// Recall at 3 and MRR are the means of the 2 prompts with a relevant note. The false injection rate is for the 2 prompts with none.
	want := [][]string{
		{"text", "0.75", "0.75", "0.50"},   // recall (1 + 1/2) / 2, MRR (1 + 1/2) / 2, 1 of 2
		{"vector", "0.50", "0.60", "0.00"}, // recall (0 + 1) / 2, MRR (1/5 + 1) / 2, 0 of 2
		{"hybrid", "1.00", "1.00", "0.50"}, // recall (1 + 1) / 2, MRR (1 + 1) / 2, 1 of 2
	}
	for i, w := range want {
		got := strings.Fields(lines[i+1])
		if len(got) != 6 || !slices.Equal(got[:4], w) {
			t.Errorf("row %d: got %q, want %q and the two latency values", i+1, got, w)
		}
	}

	// One hybrid request with the first prompt comes before the measurement. It opens the connections to the two services.
	// The full-text form makes no embedding request: 1 request and 4 prompts in 2 forms.
	if len(f.embeds) != 9 {
		t.Errorf("embedding requests: got %d, want 9", len(f.embeds))
	}
	var text, vector, hybrid int
	for _, q := range f.queries {
		hasVector, hasText := len(q.QueryVector.Float32) > 0, len(q.QueryTexts) > 0
		switch {
		case hasVector && hasText:
			hybrid++
		case hasVector:
			vector++
		case hasText:
			text++
		}
		if project, _ := q.Filter["project"].(map[string]any); project["$ne"] != "proj-current" || q.TopK != 10 {
			t.Errorf("request: got filter %v and topK %d, want the filter on proj-current and 10", q.Filter, q.TopK)
		}
	}
	if text != 4 || vector != 4 || hybrid != 5 {
		t.Errorf("QueryVectors requests: got %d text, %d vector and %d hybrid, want 4, 4 and 5", text, vector, hybrid)
	}
}

func TestEvalErrors(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		failQuery bool
		wantError string // a part of the error text
	}{
		{name: "no file argument", args: []string{"eval"}, wantError: "usage: krakozhia eval <test set file>"},
		{name: "file is absent", args: []string{"eval", "absent.jsonl"}, wantError: "absent.jsonl"},
		{name: "line is not JSON", args: []string{"eval", "bad.jsonl"}, wantError: "line 2"},
		// With a wrong key name, a prompt with a relevant note counts as a prompt with none.
		{name: "line has an unknown key", args: []string{"eval", "typo.jsonl"}, wantError: "relevent"},
		{name: "file has no prompt", args: []string{"eval", "empty.jsonl"}, wantError: "no prompt"},
		{name: "query request fails", args: []string{"eval", "testset.jsonl"}, failQuery: true, wantError: "query vectors"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newFake(t)
			f.failQuery = tt.failQuery
			recallHome(t)
			dir := t.TempDir()
			t.Chdir(dir)
			writeFiles(t, dir, map[string]string{
				"testset.jsonl": evalTestSet,
				"bad.jsonl":     "{\"prompt\": \"p1\"}\nnot JSON\n",
				"typo.jsonl":    "{\"prompt\": \"p1\", \"project\": \"proj-current\", \"relevent\": [\"proj-a/alpha\"]}\n",
				"empty.jsonl":   "\n",
			})

			err := run(tt.args, strings.NewReader(""), io.Discard)

			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("run: got error %v, want an error with the text %q", err, tt.wantError)
			}
		})
	}
}
