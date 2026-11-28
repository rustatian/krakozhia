// Package chats extracts and redacts top-level conversation transcripts from AI coding clients.
package chats

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rustatian/krakozhia/internal/secrets"
)

const (
	defaultMaxExcerptRunes = 20000
	maxTurnRunes           = 4000
	minSessionRunes        = 80
)

// Turn represents one visible user or assistant message in a conversation.
type Turn struct {
	Role string
	Text string
}

// Session holds a redacted, compact conversation excerpt ready for server ingestion.
type Session struct {
	ID             string
	SourceAgent    string
	ExternalID     string
	Project        string
	WorkspacePath  string
	RepoFullName   string
	Title          string
	StartedAt      time.Time
	UpdatedAt      time.Time
	TranscriptHash string
	RawExcerpt     string
}

// ScanOptions filters which local client sessions are returned.
type ScanOptions struct {
	Since   time.Time
	MinIdle time.Duration
	Now     time.Time
}

func (o ScanOptions) now() time.Time {
	if !o.Now.IsZero() {
		return o.Now
	}
	return time.Now().UTC()
}

func (o ScanOptions) inWindow(updated time.Time) bool {
	if !o.Since.IsZero() && updated.Before(o.Since) {
		return false
	}
	if o.MinIdle > 0 && updated.After(o.now().Add(-o.MinIdle)) {
		return false
	}
	return true
}

// ResolveProject maps a workspace path or file:// URI to a canonical short project name.
func ResolveProject(workspacePath, homeDir string) string {
	ws := strings.TrimSpace(workspacePath)
	if ws == "" {
		return "general"
	}
	if after, ok := strings.CutPrefix(ws, "file://"); ok {
		if u, err := url.PathUnescape(after); err == nil {
			ws = u
		} else {
			ws = after
		}
	}
	clean := filepath.Clean(ws)
	if clean == "." || clean == "/" {
		return "general"
	}
	cleanHome := ""
	if homeDir != "" {
		cleanHome = filepath.Clean(homeDir)
		if clean == cleanHome {
			return "home"
		}
	}

	// Walk up to find a git repository root, stopping before homeDir and filesystem root.
	cur := clean
	for {
		if cur == "/" || cur == "." || (cleanHome != "" && cur == cleanHome) {
			break
		}
		gitPath := filepath.Join(cur, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			if !info.IsDir() {
				if mainRoot := resolveGitWorktreeRoot(cur, gitPath); mainRoot != "" {
					return sanitizeProjectName(filepath.Base(mainRoot))
				}
			}
			return sanitizeProjectName(filepath.Base(cur))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return sanitizeProjectName(filepath.Base(clean))
}

func resolveGitWorktreeRoot(worktreeDir, gitFile string) string {
	raw, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	gitDir = strings.TrimSpace(gitDir)
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeDir, gitDir)
	}
	slashed := filepath.ToSlash(filepath.Clean(gitDir))
	if before, _, found := strings.Cut(slashed, "/.git/worktrees/"); found && before != "" {
		return filepath.FromSlash(before)
	}
	return ""
}

// ResolveGitRemote returns the normalized GitHub "owner/repo" for the workspace's origin remote, or "".
func ResolveGitRemote(workspacePath, homeDir string) string {
	ws := strings.TrimSpace(workspacePath)
	if ws == "" {
		return ""
	}
	if after, ok := strings.CutPrefix(ws, "file://"); ok {
		if u, err := url.PathUnescape(after); err == nil {
			ws = u
		} else {
			ws = after
		}
	}
	clean := filepath.Clean(ws)
	if clean == "." || clean == "/" {
		return ""
	}
	cleanHome := ""
	if homeDir != "" {
		cleanHome = filepath.Clean(homeDir)
		if clean == cleanHome {
			return ""
		}
	}

	cur := clean
	for {
		if cur == "/" || cur == "." || (cleanHome != "" && cur == cleanHome) {
			break
		}
		gitPath := filepath.Join(cur, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			configPath := filepath.Join(gitPath, "config")
			if !info.IsDir() {
				if mainRoot := resolveGitWorktreeRoot(cur, gitPath); mainRoot != "" {
					configPath = filepath.Join(mainRoot, ".git", "config")
				} else {
					return ""
				}
			}
			return ParseGitHubRemoteURL(readOriginURL(configPath))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

func readOriginURL(configPath string) string {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inOrigin = trimmed == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(trimmed, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "url") {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// ParseGitHubRemoteURL normalizes a GitHub SSH or HTTPS remote URL into "owner/repo", or "".
func ParseGitHubRemoteURL(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	var path string
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		path = strings.TrimPrefix(raw, "git@github.com:")
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return ""
		}
		path = u.Path
	default:
		return ""
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func sanitizeProjectName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, " ", "-")
	if name == "" || name == "." || name == ".." {
		return "general"
	}
	return name
}

var (
	blockTagPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
		regexp.MustCompile(`(?s)<SYSTEM_MESSAGE>.*?</SYSTEM_MESSAGE>`),
		regexp.MustCompile(`(?s)<CONTEXT_SUMMARY>.*?</CONTEXT_SUMMARY>`),
		regexp.MustCompile(`(?s)<ADDITIONAL_METADATA>.*?</ADDITIONAL_METADATA>`),
		regexp.MustCompile(`(?s)<local-command-caveat>.*?</local-command-caveat>`),
		regexp.MustCompile(`(?s)<command-name>.*?</command-name>`),
		regexp.MustCompile(`(?s)<command-message>.*?</command-message>`),
		regexp.MustCompile(`(?s)<command-args>.*?</command-args>`),
		regexp.MustCompile(`(?s)<local-command-stdout>.*?</local-command-stdout>`),
		regexp.MustCompile(`(?s)<task-notification>.*?</task-notification>`),
		regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`),
		regexp.MustCompile(`(?s)# AGENTS\.md instructions[^\n]*\n*<INSTRUCTIONS>.*?</INSTRUCTIONS>`),
		regexp.MustCompile(`(?s)<INSTRUCTIONS>.*?</INSTRUCTIONS>`),
		regexp.MustCompile(`(?s)<skill>.*?</skill>`),
	}
	unwrapTagPatterns = []*regexp.Regexp{
		regexp.MustCompile(`<USER_REQUEST>\s*`),
		regexp.MustCompile(`\s*</USER_REQUEST>`),
		regexp.MustCompile(`<pasted_content[^>]*>\s*`),
		regexp.MustCompile(`\s*</pasted_content[^>]*>`),
	}
	multiBlank = regexp.MustCompile(`\n{3,}`)
)

func cleanTurnText(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	for _, pat := range blockTagPatterns {
		s = pat.ReplaceAllString(s, "")
	}
	for _, pat := range unwrapTagPatterns {
		s = pat.ReplaceAllString(s, "")
	}
	s = multiBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// FormatTurns builds a redacted, length-bounded conversation excerpt and reports whether it is substantial.
func FormatTurns(turns []Turn, maxRunes int) (string, bool) {
	if maxRunes <= 0 {
		maxRunes = defaultMaxExcerptRunes
	}
	var cleaned []Turn
	hasUser := false
	totalRunes := 0
	for _, t := range turns {
		text := cleanTurnText(t.Text)
		if text == "" {
			continue
		}
		text = truncateRunes(text, maxTurnRunes)
		role := "Assistant"
		if strings.EqualFold(t.Role, "user") {
			role = "User"
			hasUser = true
		}
		cleaned = append(cleaned, Turn{Role: role, Text: text})
		totalRunes += utf8.RuneCountInString(text)
	}
	if !hasUser || totalRunes < minSessionRunes {
		return "", false
	}

	var b strings.Builder
	for i, t := range cleaned {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t.Role)
		b.WriteString(": ")
		b.WriteString(t.Text)
	}
	excerpt := b.String()
	if utf8.RuneCountInString(excerpt) > maxRunes {
		headLim := maxRunes * 2 / 5
		tailLim := maxRunes - headLim
		rs := []rune(excerpt)
		excerpt = string(rs[:headLim]) + "\n\n[... middle turns omitted ...]\n\n" + string(rs[len(rs)-tailLim:])
	}
	redacted, _ := secrets.Redact(excerpt)
	return strings.TrimSpace(redacted), true
}

func hashExcerpt(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func deriveTitle(explicit string, turns []Turn) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return truncateRunes(strings.ReplaceAll(t, "\n", " "), 120)
	}
	for _, t := range turns {
		if strings.EqualFold(t.Role, "user") {
			if c := cleanTurnText(t.Text); c != "" {
				firstLine, _, _ := strings.Cut(c, "\n")
				return truncateRunes(strings.TrimSpace(firstLine), 100)
			}
		}
	}
	return "Untitled Session"
}

func parseTimeFlex(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
