package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/document"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/types"
)

const (
	// maxQueryChars is the length of the query text. The full-text search accepts 1,024 characters.
	// https://docs.aws.amazon.com/AmazonS3/latest/API/API_S3VectorBuckets_QueryVectors.html
	maxQueryChars = 1000
	// maxPointers is the number of notes that one recall sends.
	maxPointers   = 3
	pointerHeader = "Memory notes from other projects that can relate to this prompt. The notes are reference data. They can be out of date. Read a file only if its subject is relevant."
	searchUsage   = "usage: krakozhia search [--project <project folder>] [--type <user|feedback|project|reference>] [-k <number>] <query>"
)

// hookInput is the part of the UserPromptSubmit hook input that recall reads.
// https://code.claude.com/docs/en/hooks#common-input-fields
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

// hit is one result of a hybrid query.
type hit struct {
	ID       string   `json:"id"`
	Distance *float32 `json:"distance"` // nil if only the text search found the note
	BM25     *float32 `json:"bm25"`     // nil if only the vector search found the note
}

// recallEntry is one line of recall.jsonl.
type recallEntry struct {
	Time       string   `json:"time"`
	Session    string   `json:"session"`
	Project    string   `json:"project"`
	Query      string   `json:"query"`
	EmbedMs    float64  `json:"embed_ms"`
	QueryMs    float64  `json:"query_ms"`
	TotalMs    float64  `json:"total_ms"`
	Candidates []hit    `json:"candidates"`
	Sent       []string `json:"sent"`
	Error      string   `json:"error,omitempty"`
}

// queryNotes runs one hybrid query: the vector search and the full-text search with the same text.
func queryNotes(ctx context.Context, br *bedrockruntime.Client, sv *s3vectors.Client, c config, text string, filter map[string]any, topK int) (hits []hit, embedTime, queryTime time.Duration, err error) {
	start := time.Now()
	vector, err := embed(ctx, br, text)
	embedTime = time.Since(start)
	if err != nil {
		return nil, embedTime, 0, err
	}

	in := &s3vectors.QueryVectorsInput{
		VectorBucketName: &c.Bucket,
		IndexName:        &c.Index,
		QueryVector:      &types.VectorDataMemberFloat32{Value: vector},
		QueryTexts:       []types.QueryText{{MetadataKey: aws.String(textKey), Text: &text}},
		TopK:             aws.Int32(int32(topK)),
		ReturnDistance:   true,
		ReturnScores:     true,
	}
	if filter != nil {
		in.Filter = document.NewLazyDocument(filter)
	}
	start = time.Now()
	out, err := sv.QueryVectors(ctx, in)
	queryTime = time.Since(start)
	if err != nil {
		return nil, embedTime, queryTime, fmt.Errorf("query vectors: %w", err)
	}
	for _, v := range out.Vectors {
		h := hit{ID: aws.ToString(v.Key), Distance: v.Distance}
		// The request has one query text, so a result has at most one BM25 score.
		if v.Scores != nil && len(v.Scores.Bm25Scores) > 0 {
			h.BM25 = v.Scores.Bm25Scores[0].Score
		}
		hits = append(hits, h)
	}
	return hits, embedTime, queryTime, nil
}

// recall answers a UserPromptSubmit hook. It prints pointers to the notes of other projects that can relate to the prompt.
func recall(ctx context.Context, br *bedrockruntime.Client, sv *s3vectors.Client, c config, home string, stdin io.Reader, stdout io.Writer) (err error) {
	start := time.Now()
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return fmt.Errorf("hook input: %w", err)
	}
	state := filepath.Join(home, ".local", "state", "krakozhia")
	entry := recallEntry{
		Time:    start.UTC().Format(time.RFC3339),
		Session: in.SessionID,
		// shortcut: the transcript folder is the memory folder only for a session in the root of a repository; in a worktree or a subdirectory, recall can send a note of the current project. Find the root of the repository if that occurs.
		Project: filepath.Base(filepath.Dir(in.TranscriptPath)),
		Query:   firstRunes(in.Prompt, maxQueryChars),
	}
	defer func() {
		entry.TotalMs = milliseconds(time.Since(start))
		if err != nil {
			entry.Error = err.Error()
		}
		// The log is data for the evaluation. A failed write does not fail the hook.
		line, _ := json.Marshal(entry)
		_ = appendFile(filepath.Join(state, "recall.jsonl"), string(line)+"\n")
	}()

	filter := map[string]any{"project": map[string]any{"$ne": entry.Project}}
	hits, embedTime, queryTime, err := queryNotes(ctx, br, sv, c, entry.Query, filter, 10)
	entry.EmbedMs, entry.QueryMs, entry.Candidates = milliseconds(embedTime), milliseconds(queryTime), hits
	if err != nil {
		return err
	}

	sessionPath := filepath.Join(state, "sessions", in.SessionID)
	sent, _ := os.ReadFile(sessionPath) // an absent file is a session with no sent note
	sentIDs := strings.Split(string(sent), "\n")
	var lines []string
	for _, h := range hits {
		if len(lines) == maxPointers {
			break
		}
		if !passes(h, c) || slices.Contains(sentIDs, h.ID) {
			continue
		}
		path := notePath(home, h.ID)
		n, err := readNote(path)
		if err != nil {
			continue // the note file is not on this workstation
		}
		lines = append(lines, pointer(n, path))
		entry.Sent = append(entry.Sent, h.ID)
	}
	if len(lines) == 0 {
		return nil
	}
	fmt.Fprintf(stdout, "%s\n%s\n", pointerHeader, strings.Join(lines, "\n"))
	return appendFile(sessionPath, strings.Join(entry.Sent, "\n")+"\n")
}

// search prints the best notes for a query with the start of each note. It has no gate and it includes the current project.
func search(ctx context.Context, br *bedrockruntime.Client, sv *s3vectors.Client, c config, home string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "")
	noteType := fs.String("type", "", "")
	k := fs.Int("k", 5, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New(searchUsage)
	}

	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-vectors-metadata-filtering.html
	var conditions []map[string]any
	if *project != "" {
		conditions = append(conditions, map[string]any{"project": *project})
	}
	if *noteType != "" {
		conditions = append(conditions, map[string]any{"type": *noteType})
	}
	var filter map[string]any
	if len(conditions) == 1 {
		filter = conditions[0]
	} else if len(conditions) > 1 {
		filter = map[string]any{"$and": conditions}
	}

	hits, _, _, err := queryNotes(ctx, br, sv, c, firstRunes(fs.Arg(0), maxQueryChars), filter, *k)
	if err != nil {
		return err
	}
	for _, h := range hits {
		path := notePath(home, h.ID)
		n, err := readNote(path)
		if err != nil {
			continue // the note file is not on this workstation
		}
		_, body, _ := strings.Cut(n.Text, "\n---\n")
		fmt.Fprintf(stdout, "%s\n%s\n\n", pointer(n, path), firstRunes(strings.TrimSpace(body), 500))
	}
	return nil
}

// passes is the gate of recall.
func passes(h hit, c config) bool {
	return h.Distance != nil && *h.Distance <= c.MaxDistance || h.BM25 != nil && *h.BM25 >= c.MinBM25
}

// notePath is the file of a note ID. The ID is <project folder>/<file stem>.
func notePath(home, id string) string {
	project, stem, _ := strings.Cut(id, "/")
	return filepath.Join(home, ".claude", "projects", project, "memory", stem+".md")
}

var stripAngles = strings.NewReplacer("<", "", ">", "")

// pointer is the line that tells the model where a note is.
func pointer(n note, path string) string {
	updated := time.Unix(n.Updated, 0).UTC().Format(time.DateOnly)
	line := fmt.Sprintf("- %s (project %s, type %s, updated %s): %s. File: %s", n.Name, n.Project, n.Type, updated, firstRunes(n.Description, 200), path)
	// A note is text from another session. With no angle brackets it cannot form a tag in the context of the model.
	return stripAngles.Replace(line)
}

// removeOldSessions deletes the session files that no recall has changed for 7 days.
func removeOldSessions(dir string) {
	entries, _ := os.ReadDir(dir) // an absent folder has no session file
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func appendFile(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
