// Package chunk splits Markdown documents into heading-indexed sections for search.
package chunk

import (
	"strings"
	"unicode/utf8"
)

const maxSectionRunes = 6000

// Section holds one searchable slice of a Markdown document.
type Section struct {
	Index       int
	HeadingPath string
	Content     string
}

// EmbedText returns the heading path followed by the section body for embedding.
func (s Section) EmbedText() string {
	if s.HeadingPath == "" {
		return s.Content
	}
	return s.HeadingPath + "\n\n" + s.Content
}

// Title returns the text of the first level-1 Markdown heading outside code fences.
func Title(markdown string) string {
	var inFence bool
	var fenceChar byte
	for raw := range strings.SplitSeq(markdown, "\n") {
		line := strings.TrimRight(raw, "\r")
		if toggled, ch := updateFence(line, inFence, fenceChar); toggled != inFence {
			inFence = toggled
			fenceChar = ch
			continue
		}
		if inFence {
			continue
		}
		if level, heading := parseHeading(line); level == 1 {
			return heading
		}
	}
	return ""
}

// Split divides markdown at H1 to H3 headings outside code fences and caps each section at 6,000 runes.
func Split(markdown, fallbackTitle string) []Section {
	var (
		sections  []Section
		bodyLines []string
		inFence   bool
		fenceChar byte
		h1        = strings.TrimSpace(fallbackTitle)
		h2, h3    string
	)

	flush := func() {
		body := strings.TrimSpace(strings.Join(bodyLines, "\n"))
		bodyLines = bodyLines[:0]
		if body == "" {
			return
		}
		path := joinPath(h1, h2, h3)
		for _, part := range splitByRunes(body, maxSectionRunes) {
			sections = append(sections, Section{
				Index:       len(sections),
				HeadingPath: path,
				Content:     part,
			})
		}
	}

	for raw := range strings.SplitSeq(markdown, "\n") {
		line := strings.TrimRight(raw, "\r")
		if toggled, ch := updateFence(line, inFence, fenceChar); toggled != inFence {
			inFence = toggled
			fenceChar = ch
			bodyLines = append(bodyLines, line)
			continue
		}
		if inFence {
			bodyLines = append(bodyLines, line)
			continue
		}
		level, heading := parseHeading(line)
		switch level {
		case 1:
			flush()
			h1 = heading
			h2, h3 = "", ""
		case 2:
			flush()
			h2 = heading
			h3 = ""
		case 3:
			flush()
			h3 = heading
		default:
			bodyLines = append(bodyLines, line)
		}
	}
	flush()
	return sections
}

func updateFence(line string, inFence bool, fenceChar byte) (bool, byte) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return inFence, fenceChar
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return inFence, fenceChar
	}
	if trimmed[1] != ch || trimmed[2] != ch {
		return inFence, fenceChar
	}
	if !inFence {
		return true, ch
	}
	if ch == fenceChar {
		return false, 0
	}
	return inFence, fenceChar
}

func parseHeading(line string) (int, string) {
	trimmed := strings.TrimSpace(line)
	for level := 1; level <= 3; level++ {
		prefix := strings.Repeat("#", level) + " "
		if rest, ok := strings.CutPrefix(trimmed, prefix); ok {
			return level, strings.TrimSpace(rest)
		}
	}
	return 0, ""
}

func joinPath(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " > ")
}

func splitByRunes(text string, limit int) []string {
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	var out []string
	var cur strings.Builder
	curRunes := 0
	for para := range strings.SplitSeq(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		paraRunes := utf8.RuneCountInString(para)
		if curRunes > 0 && curRunes+2+paraRunes <= limit {
			cur.WriteString("\n\n")
			cur.WriteString(para)
			curRunes += 2 + paraRunes
			continue
		}
		if curRunes > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curRunes = 0
		}
		if paraRunes <= limit {
			cur.WriteString(para)
			curRunes = paraRunes
			continue
		}
		runes := []rune(para)
		for len(runes) > 0 {
			n := min(limit, len(runes))
			out = append(out, string(runes[:n]))
			runes = runes[n:]
		}
	}
	if curRunes > 0 {
		out = append(out, cur.String())
	}
	return out
}
