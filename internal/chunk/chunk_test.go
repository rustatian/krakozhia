package chunk_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rustatian/krakozhia/internal/chunk"
)

func TestTitle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		md   string
		want string
	}{
		{
			name: "first level one heading",
			md:   "intro\n# Central Knowledge Base\n## Summary\n",
			want: "Central Knowledge Base",
		},
		{
			name: "no level one heading",
			md:   "## Summary\n\nBody text.\n",
			want: "",
		},
		{
			name: "ignore hash inside fenced code block",
			md:   "```sh\n# shell comment\n```\n# Real Title\n",
			want: "Real Title",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chunk.Title(tc.md); got != tc.want {
				t.Fatalf("Title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()

	p1 := strings.Repeat("a", 3500)
	p2 := strings.Repeat("b", 3500)
	huge := strings.Repeat("c", 7000)

	cases := []struct {
		name          string
		md            string
		fallbackTitle string
		want          []chunk.Section
	}{
		{
			name:          "nested headings drop empty parent",
			md:            "# Design\n\n## Architecture\n### Storage\nPostgres holds chunks.\n\n### Search\nHybrid RRF.\n",
			fallbackTitle: "design",
			want: []chunk.Section{
				{Index: 0, HeadingPath: "Design > Architecture > Storage", Content: "Postgres holds chunks."},
				{Index: 1, HeadingPath: "Design > Architecture > Search", Content: "Hybrid RRF."},
			},
		},
		{
			name:          "fenced code hash ignored as heading",
			md:            "# Guide\n\n```bash\n# not a heading\necho ok\n```\n",
			fallbackTitle: "guide",
			want: []chunk.Section{
				{Index: 0, HeadingPath: "Guide", Content: "```bash\n# not a heading\necho ok\n```"},
			},
		},
		{
			name:          "document without h1 uses fallback title",
			md:            "Plain intro paragraph.\n\n## Details\nBody here.\n",
			fallbackTitle: "notes",
			want: []chunk.Section{
				{Index: 0, HeadingPath: "notes", Content: "Plain intro paragraph."},
				{Index: 1, HeadingPath: "notes > Details", Content: "Body here."},
			},
		},
		{
			name:          "section above 6000 runes splits on paragraph boundary",
			md:            "# Bench\n\n## Run\n" + p1 + "\n\n" + p2 + "\n",
			fallbackTitle: "bench",
			want: []chunk.Section{
				{Index: 0, HeadingPath: "Bench > Run", Content: p1},
				{Index: 1, HeadingPath: "Bench > Run", Content: p2},
			},
		},
		{
			name:          "single oversized paragraph splits at 6000 runes",
			md:            "# Big\n" + huge + "\n",
			fallbackTitle: "big",
			want: []chunk.Section{
				{Index: 0, HeadingPath: "Big", Content: strings.Repeat("c", 6000)},
				{Index: 1, HeadingPath: "Big", Content: strings.Repeat("c", 1000)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := chunk.Split(tc.md, tc.fallbackTitle)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Split() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
