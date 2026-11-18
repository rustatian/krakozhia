package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/document"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/types"
)

// manifest records what the index holds: the content hash of each note that sync wrote.
type manifest struct {
	Index  string            `json:"index"`
	Hashes map[string]string `json:"hashes"`
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// planSync compares the notes with the manifest.
// A manifest of another index says nothing about this index, so all notes are written.
func planSync(notes []note, m manifest, index string) (puts []note, deletes []string) {
	if m.Index != index {
		return notes, nil
	}
	onDisk := make(map[string]bool, len(notes))
	for _, n := range notes {
		onDisk[n.ID] = true
		if m.Hashes[n.ID] != hashText(n.Text) {
			puts = append(puts, n)
		}
	}
	for id := range m.Hashes {
		if !onDisk[id] {
			deletes = append(deletes, id)
		}
	}
	slices.Sort(deletes)
	return puts, deletes
}

// readManifest returns an empty manifest when the file is absent or damaged. sync then writes all notes again.
// shortcut: after a lost manifest, the vectors of deleted notes stay in the index; list the index keys if that occurs.
func readManifest(path string) manifest {
	var m manifest
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &m) != nil || m.Hashes == nil {
		return manifest{Hashes: map[string]string{}}
	}
	return m
}

func writeManifest(path string, m manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// The rename replaces the file in one step, so a second sync process never reads a part of it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// putBatch is the number of notes in one PutVectors request. The API limit is 500.
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-vectors-limitations.html
// A small batch records the progress of a long first sync.
const putBatch = 50

// syncNotes makes the index hold the given notes.
// The manifest records each write that succeeded, so the next run repeats only the failed part.
func syncNotes(ctx context.Context, br *bedrockruntime.Client, sv *s3vectors.Client, c config, notes []note, manifestPath string) (written, deleted int, errs []error) {
	m := readManifest(manifestPath)
	puts, deletes := planSync(notes, m, c.Index)
	if m.Index != c.Index {
		m = manifest{Index: c.Index, Hashes: map[string]string{}}
	}

	if len(deletes) > 0 {
		_, err := sv.DeleteVectors(ctx, &s3vectors.DeleteVectorsInput{VectorBucketName: &c.Bucket, IndexName: &c.Index, Keys: deletes})
		if err != nil {
			errs = append(errs, fmt.Errorf("delete vectors: %w", err))
		} else {
			for _, id := range deletes {
				delete(m.Hashes, id)
			}
			deleted = len(deletes)
			if err := writeManifest(manifestPath, m); err != nil {
				return written, deleted, append(errs, err)
			}
		}
	}

	for batch := range slices.Chunk(puts, putBatch) {
		var embedded []note
		var vectors []types.PutInputVector
		for _, n := range batch {
			vector, err := embed(ctx, br, n.Text)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n.ID, err))
				continue
			}
			embedded = append(embedded, n)
			vectors = append(vectors, types.PutInputVector{
				Key:  &n.ID,
				Data: &types.VectorDataMemberFloat32{Value: vector},
				Metadata: document.NewLazyDocument(map[string]any{
					"project": n.Project,
					"type":    n.Type,
					"updated": n.Updated,
					textKey:   n.Text,
				}),
			})
		}
		if len(vectors) == 0 {
			continue
		}
		if _, err := sv.PutVectors(ctx, &s3vectors.PutVectorsInput{VectorBucketName: &c.Bucket, IndexName: &c.Index, Vectors: vectors}); err != nil {
			errs = append(errs, fmt.Errorf("put vectors: %w", err))
			continue
		}
		for _, n := range embedded {
			m.Hashes[n.ID] = hashText(n.Text)
		}
		written += len(embedded)
		if err := writeManifest(manifestPath, m); err != nil {
			return written, deleted, append(errs, err)
		}
	}
	return written, deleted, errs
}
