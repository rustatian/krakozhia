package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type embedCall struct {
	Path       string
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type putCall struct {
	VectorBucketName string `json:"vectorBucketName"`
	IndexName        string `json:"indexName"`
	Vectors          []struct {
		Key  string `json:"key"`
		Data struct {
			Float32 []float32 `json:"float32"`
		} `json:"data"`
		Metadata map[string]any `json:"metadata"`
	} `json:"vectors"`
}

type deleteCall struct {
	VectorBucketName string   `json:"vectorBucketName"`
	IndexName        string   `json:"indexName"`
	Keys             []string `json:"keys"`
}

type queryText struct {
	MetadataKey string `json:"metadataKey"`
	Text        string `json:"text"`
}

type queryCall struct {
	VectorBucketName string `json:"vectorBucketName"`
	IndexName        string `json:"indexName"`
	QueryVector      struct {
		Float32 []float32 `json:"float32"`
	} `json:"queryVector"`
	QueryTexts     []queryText    `json:"queryTexts"`
	Filter         map[string]any `json:"filter"`
	TopK           int            `json:"topK"`
	ReturnDistance bool           `json:"returnDistance"`
	ReturnScores   bool           `json:"returnScores"`
	ReturnMetadata bool           `json:"returnMetadata"`
}

// fakeAWS answers the Bedrock and S3 Vectors requests of the SDK clients and records them.
type fakeAWS struct {
	rejectText string        // the embedding model rejects this input text
	delay      time.Duration // wait before the answer to an embedding request
	failPut    bool
	failDelete bool
	failQuery  bool
	hits       []hit // the answer to a QueryVectors request

	embeds  []embedCall
	puts    []putCall
	deletes []deleteCall
	creates []map[string]any
	queries []queryCall
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	dec := json.NewDecoder(r.Body)
	switch {
	case strings.HasPrefix(r.URL.Path, "/model/"):
		call := embedCall{Path: r.URL.Path}
		_ = dec.Decode(&call)
		f.embeds = append(f.embeds, call)
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
		if call.InputText == f.rejectText {
			reject(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": fakeVector()})
	case r.URL.Path == "/PutVectors":
		var call putCall
		_ = dec.Decode(&call)
		f.puts = append(f.puts, call)
		if f.failPut {
			reject(w)
			return
		}
		fmt.Fprint(w, "{}")
	case r.URL.Path == "/DeleteVectors":
		var call deleteCall
		_ = dec.Decode(&call)
		f.deletes = append(f.deletes, call)
		if f.failDelete {
			reject(w)
			return
		}
		fmt.Fprint(w, "{}")
	case r.URL.Path == "/CreateIndex":
		var call map[string]any
		_ = dec.Decode(&call)
		f.creates = append(f.creates, call)
		fmt.Fprint(w, "{}")
	case r.URL.Path == "/QueryVectors":
		var call queryCall
		_ = dec.Decode(&call)
		f.queries = append(f.queries, call)
		if f.failQuery {
			reject(w)
			return
		}
		vectors := []map[string]any{}
		for _, h := range f.hits {
			v := map[string]any{"key": h.ID}
			scores := map[string]any{}
			if h.Distance != nil {
				v["distance"] = *h.Distance
				scores["vectorDistance"] = *h.Distance
			}
			if h.BM25 != nil {
				scores["bm25Scores"] = []map[string]any{{"metadataKey": "text", "score": *h.BM25}}
			}
			v["scores"] = scores
			vectors = append(vectors, v)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vectors, "distanceMetric": "cosine"})
	default:
		http.NotFound(w, r)
	}
}

// reject answers with an error that the SDK does not retry.
func reject(w http.ResponseWriter) {
	w.Header().Set("X-Amzn-Errortype", "ValidationException")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprint(w, `{"message":"rejected by the test"}`)
}

func fakeVector() []float32 {
	v := make([]float32, 1024)
	v[0], v[1023] = 0.5, -0.25
	return v
}

// newFake starts the fake and points the AWS SDK at it. The SDK reads no file of the real ~/.aws folder.
func newFake(t *testing.T) (*fakeAWS, config) {
	t.Helper()
	f := &fakeAWS{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	return f, config{Region: "eu-central-1", Bucket: "memory", Index: "idx"}
}

func TestSyncNotes(t *testing.T) {
	a := note{ID: "proj-a/a", Project: "proj-a", Type: "project", Name: "a", Description: "A", Updated: 1790000000, Text: "text of a"}
	b := note{ID: "proj-b/b", Project: "proj-b", Type: "feedback", Name: "b", Description: "B", Updated: 1790000001, Text: "text of b"}
	hashA, hashB := hashText("text of a"), hashText("text of b")

	tests := []struct {
		name        string
		notes       []note
		manifest    manifest // the manifest file before the run; no file if the index is empty
		rejectText  string
		failPut     bool
		failDelete  bool
		wantEmbeds  []string   // input texts of the embedding requests
		wantPuts    [][]string // keys of each PutVectors request
		wantDeletes [][]string // keys of each DeleteVectors request
		wantWritten int
		wantDeleted int
		wantErrs    int
		wantHashes  map[string]string // the manifest file after the run
	}{
		{
			name:        "first sync writes all notes",
			notes:       []note{a, b},
			wantEmbeds:  []string{"text of a", "text of b"},
			wantPuts:    [][]string{{"proj-a/a", "proj-b/b"}},
			wantWritten: 2,
			wantHashes:  map[string]string{"proj-a/a": hashA, "proj-b/b": hashB},
		},
		{
			name:       "no change makes no request",
			notes:      []note{a},
			manifest:   manifest{Index: "idx", Hashes: map[string]string{"proj-a/a": hashA}},
			wantHashes: map[string]string{"proj-a/a": hashA},
		},
		{
			name:        "changed note is written and a note with no file is deleted",
			notes:       []note{a, b},
			manifest:    manifest{Index: "idx", Hashes: map[string]string{"proj-a/a": hashA, "proj-b/b": hashText("old text of b"), "proj-c/gone": hashText("x")}},
			wantEmbeds:  []string{"text of b"},
			wantPuts:    [][]string{{"proj-b/b"}},
			wantDeletes: [][]string{{"proj-c/gone"}},
			wantWritten: 1,
			wantDeleted: 1,
			wantHashes:  map[string]string{"proj-a/a": hashA, "proj-b/b": hashB},
		},
		{
			name:        "note that the model rejects is skipped",
			notes:       []note{a, b},
			manifest:    manifest{Index: "idx", Hashes: map[string]string{}},
			rejectText:  "text of a",
			wantEmbeds:  []string{"text of a", "text of b"},
			wantPuts:    [][]string{{"proj-b/b"}},
			wantWritten: 1,
			wantErrs:    1,
			wantHashes:  map[string]string{"proj-b/b": hashB},
		},
		{
			name:       "failed write keeps the old hash",
			notes:      []note{a},
			manifest:   manifest{Index: "idx", Hashes: map[string]string{"proj-a/a": hashText("old text of a")}},
			failPut:    true,
			wantEmbeds: []string{"text of a"},
			wantPuts:   [][]string{{"proj-a/a"}},
			wantErrs:   1,
			wantHashes: map[string]string{"proj-a/a": hashText("old text of a")},
		},
		{
			name:        "failed delete keeps the manifest entry",
			notes:       []note{a},
			manifest:    manifest{Index: "idx", Hashes: map[string]string{"proj-a/a": hashA, "proj-c/gone": hashText("x")}},
			failDelete:  true,
			wantDeletes: [][]string{{"proj-c/gone"}},
			wantErrs:    1,
			wantHashes:  map[string]string{"proj-a/a": hashA, "proj-c/gone": hashText("x")},
		},
		{
			name:        "manifest of another index is replaced",
			notes:       []note{a},
			manifest:    manifest{Index: "old-idx", Hashes: map[string]string{"proj-a/a": hashA, "proj-c/gone": hashText("x")}},
			wantEmbeds:  []string{"text of a"},
			wantPuts:    [][]string{{"proj-a/a"}},
			wantWritten: 1,
			wantHashes:  map[string]string{"proj-a/a": hashA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t)
			f.rejectText, f.failPut, f.failDelete = tt.rejectText, tt.failPut, tt.failDelete
			manifestPath := filepath.Join(t.TempDir(), "manifest.json")
			if tt.manifest.Index != "" {
				if err := writeManifest(manifestPath, tt.manifest); err != nil {
					t.Fatal(err)
				}
			}
			br, sv, err := awsClients(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}

			written, deleted, errs := syncNotes(t.Context(), br, sv, c, tt.notes, manifestPath)

			if written != tt.wantWritten || deleted != tt.wantDeleted {
				t.Errorf("counts: got %d written and %d deleted, want %d and %d", written, deleted, tt.wantWritten, tt.wantDeleted)
			}
			if len(errs) != tt.wantErrs {
				t.Errorf("errors: got %v, want %d", errs, tt.wantErrs)
			}
			var embeds []string
			for _, call := range f.embeds {
				embeds = append(embeds, call.InputText)
			}
			if !slices.Equal(embeds, tt.wantEmbeds) {
				t.Errorf("embedding requests: got %q, want %q", embeds, tt.wantEmbeds)
			}
			var puts [][]string
			for _, call := range f.puts {
				var keys []string
				for _, v := range call.Vectors {
					keys = append(keys, v.Key)
				}
				puts = append(puts, keys)
			}
			if !reflect.DeepEqual(puts, tt.wantPuts) {
				t.Errorf("PutVectors requests: got %q, want %q", puts, tt.wantPuts)
			}
			var deletes [][]string
			for _, call := range f.deletes {
				deletes = append(deletes, call.Keys)
			}
			if !reflect.DeepEqual(deletes, tt.wantDeletes) {
				t.Errorf("DeleteVectors requests: got %q, want %q", deletes, tt.wantDeletes)
			}
			got := readManifest(manifestPath)
			if got.Index != "idx" || !maps.Equal(got.Hashes, tt.wantHashes) {
				t.Errorf("manifest: got %+v, want index idx and hashes %v", got, tt.wantHashes)
			}
		})
	}
}

func TestSyncNotesRequests(t *testing.T) {
	f, c := newFake(t)
	n := note{ID: "proj-a/a", Project: "proj-a", Type: "feedback", Name: "a", Description: "A", Updated: 1790000000, Text: "---\nname: a\n---\n\nBody.\n"}
	br, sv, err := awsClients(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, errs := syncNotes(t.Context(), br, sv, c, []note{n}, filepath.Join(t.TempDir(), "manifest.json")); len(errs) != 0 {
		t.Fatalf("syncNotes: %v", errs)
	}

	if len(f.embeds) != 1 || len(f.puts) != 1 || len(f.puts[0].Vectors) != 1 {
		t.Fatalf("requests: got %d embedding and %d PutVectors, want 1 and 1 with one vector", len(f.embeds), len(f.puts))
	}
	wantEmbed := embedCall{Path: "/model/amazon.titan-embed-text-v2:0/invoke", InputText: n.Text, Dimensions: 1024, Normalize: true}
	if f.embeds[0] != wantEmbed {
		t.Errorf("embedding request:\n got %+v\nwant %+v", f.embeds[0], wantEmbed)
	}
	put := f.puts[0]
	if put.VectorBucketName != "memory" || put.IndexName != "idx" {
		t.Errorf("PutVectors target: got bucket %q and index %q, want memory and idx", put.VectorBucketName, put.IndexName)
	}
	record := put.Vectors[0]
	if record.Key != "proj-a/a" {
		t.Errorf("key: got %q, want proj-a/a", record.Key)
	}
	if !slices.Equal(record.Data.Float32, fakeVector()) {
		t.Errorf("vector: got %d values, want the 1024 values of the embedding response", len(record.Data.Float32))
	}
	// JSON numbers decode to float64.
	wantMetadata := map[string]any{"project": "proj-a", "type": "feedback", "updated": float64(1790000000), "text": n.Text}
	if !reflect.DeepEqual(record.Metadata, wantMetadata) {
		t.Errorf("metadata:\n got %v\nwant %v", record.Metadata, wantMetadata)
	}
}

func TestSyncNotesBatches(t *testing.T) {
	f, c := newFake(t)
	var notes []note
	for i := range 51 {
		id := fmt.Sprintf("proj/n%02d", i)
		notes = append(notes, note{ID: id, Project: "proj", Type: "project", Text: "text of " + id})
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	br, sv, err := awsClients(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}

	written, _, errs := syncNotes(t.Context(), br, sv, c, notes, manifestPath)

	if written != 51 || len(errs) != 0 {
		t.Errorf("syncNotes: got %d written and errors %v, want 51 and no error", written, errs)
	}
	var sizes []int
	for _, call := range f.puts {
		sizes = append(sizes, len(call.Vectors))
	}
	if !slices.Equal(sizes, []int{50, 1}) {
		t.Errorf("PutVectors request sizes: got %v, want [50 1]", sizes)
	}
	if got := len(readManifest(manifestPath).Hashes); got != 51 {
		t.Errorf("manifest: got %d hashes, want 51", got)
	}
}

func TestCreateIndex(t *testing.T) {
	f, c := newFake(t)
	_, sv, err := awsClients(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}

	if err := createIndex(t.Context(), sv, c); err != nil {
		t.Fatalf("createIndex: %v", err)
	}

	want := map[string]any{
		"vectorBucketName": "memory",
		"indexName":        "idx",
		"dataType":         "float32",
		"dimension":        float64(1024),
		"distanceMetric":   "cosine",
		"metadataConfiguration": map[string]any{
			"nonFilterableMetadataKeys": []any{"text"},
			"textSearchConfigurations":  []any{map[string]any{"metadataKey": "text", "language": "en"}},
		},
	}
	if len(f.creates) != 1 || !reflect.DeepEqual(f.creates[0], want) {
		t.Errorf("CreateIndex requests:\n got %v\nwant [%v]", f.creates, want)
	}
}

func TestRunSync(t *testing.T) {
	f, _ := newFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	const valid = "---\nname: one\ndescription: A fact\nmetadata:\n  type: project\n---\n\nBody.\n"
	writeFiles(t, home, map[string]string{
		".config/krakozhia/config.json":            `{"region": "eu-central-1", "bucket": "memory"}`,
		".claude/projects/proj-a/memory/one.md":    valid,
		".claude/projects/proj-a/memory/broken.md": "# No front matter\n",
		".local/state/krakozhia/sessions/old":      "proj-a/one\n",
		".local/state/krakozhia/sessions/new":      "proj-a/one\n",
	})
	eightDaysAgo := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(home, ".local/state/krakozhia/sessions/old"), eightDaysAgo, eightDaysAgo); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"sync"}, strings.NewReader(""), io.Discard)

	if err == nil || !strings.Contains(err.Error(), "broken.md") {
		t.Errorf("run: got error %v, want an error that names broken.md", err)
	}
	if len(f.puts) != 1 || len(f.puts[0].Vectors) != 1 || f.puts[0].Vectors[0].Key != "proj-a/one" || f.puts[0].IndexName != "notes-titan-v2-1024" {
		t.Errorf("PutVectors requests: got %+v, want one record proj-a/one in the index notes-titan-v2-1024", f.puts)
	}
	got := readManifest(filepath.Join(home, ".local/state/krakozhia/manifest.json"))
	want := map[string]string{"proj-a/one": hashText(valid)}
	if got.Index != "notes-titan-v2-1024" || !maps.Equal(got.Hashes, want) {
		t.Errorf("manifest: got %+v, want index notes-titan-v2-1024 and hashes %v", got, want)
	}
	// sync removes the session files that are older than 7 days.
	entries, err := os.ReadDir(filepath.Join(home, ".local/state/krakozhia/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "new" {
		t.Errorf("session files: got %v, want only the file new", entries)
	}
}

// writeFiles writes the files below root. A key is a path relative to root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
