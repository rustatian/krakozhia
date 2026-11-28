package secrets_test

import (
	"slices"
	"testing"

	"github.com/rustatian/krakozhia/internal/secrets"
)

func TestScan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want []secrets.Finding
	}{
		{
			name: "clean technical markdown",
			text: "# Spec\n\nSHA256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nUUID: 550e8400-e29b-41d4-a716-446655440000\nENV: KRAKOZHIA_DB_DSN\nAuthorization: Bearer <token>\n",
			want: nil,
		},
		{
			name: "pem private key header",
			text: "line 1\n-----BEGIN OPENSSH PRIVATE KEY-----\nbody\n",
			want: []secrets.Finding{{Line: 2, Rule: "private-key"}},
		},
		{
			name: "aws access key id",
			text: "region=eu-central-1\nkey=" + "AKIA" + "IOSFODNN7EXAMPLE\n",
			want: []secrets.Finding{{Line: 2, Rule: "aws-access-key"}},
		},
		{
			name: "github personal access token",
			text: "token: " + "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890\n",
			want: []secrets.Finding{{Line: 1, Rule: "github-token"}},
		},
		{
			name: "github fine-grained pat",
			text: "pat=" + "github_pat_" + "11ABCDEFG0123456789_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\n",
			want: []secrets.Finding{{Line: 1, Rule: "github-token"}},
		},
		{
			name: "anthropic and openai keys",
			text: "a=" + "sk-ant-" + "api03-abcdefghijklmnopqrstuvwxyz123456\nb=" + "sk-proj-" + "abcdefghijklmnopqrstuvwxyz123456\nc=" + "sk-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234\n",
			want: []secrets.Finding{
				{Line: 1, Rule: "api-key"},
				{Line: 2, Rule: "api-key"},
				{Line: 3, Rule: "api-key"},
			},
		},
		{
			name: "slack token",
			text: "slack=" + "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx\n",
			want: []secrets.Finding{{Line: 1, Rule: "slack-token"}},
		},
		{
			name: "long bearer token in authorization header",
			text: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345\n",
			want: []secrets.Finding{{Line: 1, Rule: "bearer-token"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := secrets.Scan(tc.text)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Scan() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		input          string
		wantCount      int
		wantNotContain []string
	}{
		{
			name:      "clean text unchanged",
			input:     "Deployed pgvector 0.8.7 on wodka.",
			wantCount: 0,
		},
		{
			name:      "redacts github token and bearer header",
			input:     "tok=" + "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890\nAuthorization: Bearer abcdefghijklmnopqrstuvwxyz012345\n",
			wantCount: 2,
			wantNotContain: []string{
				"ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
				"abcdefghijklmnopqrstuvwxyz012345",
			},
		},
		{
			name:      "redacts full pem private key block",
			input:     "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE=\n-----END OPENSSH PRIVATE KEY-----\nafter\n",
			wantCount: 1,
			wantNotContain: []string{
				"BEGIN OPENSSH PRIVATE KEY",
				"b3BlbnNzaC1rZXktdjE=",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, n := secrets.Redact(tc.input)
			if n != tc.wantCount {
				t.Fatalf("Redact count = %d, want %d (out=%q)", n, tc.wantCount, got)
			}
			if findings := secrets.Scan(got); len(findings) != 0 {
				t.Fatalf("Scan after Redact found %#v in %q", findings, got)
			}
			for _, bad := range tc.wantNotContain {
				if slices.Contains([]string{got}, bad) || len(bad) > 0 && containsString(got, bad) {
					t.Fatalf("redacted text still contains %q: %q", bad, got)
				}
			}
		})
	}
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
