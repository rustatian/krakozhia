// Package secrets scans Markdown and note text for leaked credentials before storage.
package secrets

import (
	"fmt"
	"regexp"
	"strings"
)

// Finding describes one secret pattern match in a line of text.
type Finding struct {
	Line int
	Rule string
}

// FindingsError is returned when secret patterns prevent storing text.
type FindingsError struct {
	Findings []Finding
}

func (e *FindingsError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		parts = append(parts, fmt.Sprintf("line %d (%s)", f.Line, f.Rule))
	}
	return "secret pattern detected at " + strings.Join(parts, ", ")
}

type rule struct {
	name    string
	pattern *regexp.Regexp
}

var rules = []rule{
	{name: "private-key", pattern: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)},
	{name: "aws-access-key", pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", pattern: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)},
	{name: "api-key", pattern: regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{16,}|sk-proj-[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9]{20,})\b`)},
	{name: "slack-token", pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "bearer-token", pattern: regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{24,}=*\b`)},
}

var pemBlockPattern = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)

// Scan inspects text line by line and returns any matched secret patterns.
func Scan(text string) []Finding {
	var findings []Finding
	lineNo := 0
	for line := range strings.SplitSeq(text, "\n") {
		lineNo++
		for _, r := range rules {
			if r.pattern.MatchString(line) {
				findings = append(findings, Finding{Line: lineNo, Rule: r.name})
			}
		}
	}
	return findings
}

// Redact replaces matched secret blocks and tokens with placeholders and returns the replacement count.
func Redact(text string) (string, int) {
	total := 0
	out := pemBlockPattern.ReplaceAllStringFunc(text, func(string) string {
		total++
		return "[REDACTED:private-key]"
	})
	for _, r := range rules {
		placeholder := "[REDACTED:" + r.name + "]"
		out = r.pattern.ReplaceAllStringFunc(out, func(string) string {
			total++
			return placeholder
		})
	}
	return out, total
}
