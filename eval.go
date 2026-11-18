package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors"
)

const evalUsage = "usage: krakozhia eval <test set file>"

// evalCase is one line of the test set file.
type evalCase struct {
	Prompt   string   `json:"prompt"`
	Project  string   `json:"project"`  // the project folder of the session that had the prompt
	Relevant []string `json:"relevant"` // the IDs of the relevant notes. Empty for a prompt that must give no note.
}

func loadTestSet(path string) ([]evalCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []evalCase
	number := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		number++
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ec evalCase
		dec := json.NewDecoder(strings.NewReader(line))
		// With a wrong key name, a prompt with a relevant note counts as a prompt with none.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ec); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, number, err)
		}
		cases = append(cases, ec)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s: the test set has no prompt", path)
	}
	return cases, nil
}

// eval runs each prompt of a test set in the three query forms and prints the quality and the latency of each form.
// A note counts as sent if it is one of the first maxPointers candidates that pass the gate. The session state and the note files have no effect.
func eval(ctx context.Context, br *bedrockruntime.Client, sv *s3vectors.Client, c config, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New(evalUsage)
	}
	cases, err := loadTestSet(args[0])
	if err != nil {
		return err
	}
	query := func(form string, ec evalCase) ([]hit, time.Duration, error) {
		filter := map[string]any{"project": map[string]any{"$ne": ec.Project}}
		hits, embedTime, queryTime, err := queryNotes(ctx, br, sv, c, form, firstRunes(ec.Prompt, maxQueryChars), filter, candidates)
		return hits, embedTime + queryTime, err
	}
	// The first request to each service includes the connection setup and the credential request. This request keeps them out of the measurement.
	if _, _, err := query(formHybrid, cases[0]); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%-7s %-9s %-5s %-17s %-7s %s\n", "form", "recall@3", "mrr", "false-injections", "p50-ms", "p95-ms")
	for _, form := range []string{formText, formVector, formHybrid} {
		var recallSum, reciprocalRankSum float64
		var withRelevant, withNone, falseInjections int
		var times []time.Duration
		for i, ec := range cases {
			hits, elapsed, err := query(form, ec)
			if err != nil {
				return fmt.Errorf("prompt %d, %s query: %w", i+1, form, err)
			}
			times = append(times, elapsed)
			var sent []string
			for _, h := range hits {
				if len(sent) < maxPointers && passes(h, c) {
					sent = append(sent, h.ID)
				}
			}
			if len(ec.Relevant) == 0 {
				withNone++
				if len(sent) > 0 {
					falseInjections++
				}
				continue
			}
			withRelevant++
			found := 0
			for _, id := range ec.Relevant {
				if slices.Contains(sent, id) {
					found++
				}
			}
			recallSum += float64(found) / float64(len(ec.Relevant))
			// The reciprocal rank uses all candidates, so it shows the ranking without the gate.
			if rank := slices.IndexFunc(hits, func(h hit) bool { return slices.Contains(ec.Relevant, h.ID) }); rank >= 0 {
				reciprocalRankSum += 1 / float64(rank+1)
			}
		}
		fmt.Fprintf(stdout, "%-7s %-9.2f %-5.2f %-17.2f %-7.0f %.0f\n", form,
			recallSum/float64(withRelevant), reciprocalRankSum/float64(withRelevant), float64(falseInjections)/float64(withNone),
			milliseconds(percentile(times, 50)), milliseconds(percentile(times, 95)))
	}
	return nil
}

// percentile is the nearest-rank percentile: the value at rank ceil(p/100 * n) of the sorted values.
func percentile(values []time.Duration, p int) time.Duration {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[(p*len(sorted)+99)/100-1]
}
