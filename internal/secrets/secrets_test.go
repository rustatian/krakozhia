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
