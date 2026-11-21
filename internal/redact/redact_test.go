package redact

import (
	"strings"
	"testing"
)

func TestTextRedactsSecrets(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, gone, kept string }{
		"github token":   {"export GH=ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5", "ghp_aB3dE5", "export GH="},
		"fine-grained":   {"github_pat_11ABCDEFG0abcdefghij_klmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "github_pat_11", ""},
		"slack":          {"token xoxb-1234567890-abcdefghijkl", "xoxb-1234567890", ""},
		"slack hook":     {"curl https://hooks.slack.com/services/T000/B000/XXXXXXXX", "T000/B000", "curl "},
		"aws":            {"AKIAIOSFODNN7EXAMPLE is the key", "AKIAIOSFODNN7EXAMPLE", "is the key"},
		"openai":         {"OPENAI=sk-proj-abcdefghijklmnopqrstuvwxyz0123", "sk-proj-abcdef", ""},
		"anthropic":      {"sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "sk-ant-api03", ""},
		"jwt":            {"Authorization: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "dozjgNryP4J3", "Authorization"},
		"url":            {"postgres://engram:hunter2hunter2@db.internal:5432/engram", "hunter2", "postgres://"},
		"url host kept":  {"postgres://engram:hunter2hunter2@db.internal:5432/engram", "engram:hunter2", "db.internal:5432/engram"},
		"bearer":         {"curl -H 'Authorization: Bearer abcdefghijklmnop1234567890'", "abcdefghijklmnop1234567890", "Bearer "},
		"env assignment": {"ENGRAM_PASSWORD=correct-horse-battery", "correct-horse-battery", "ENGRAM_PASSWORD="},
		"yaml":           {"client_secret: s3cr3t-value-here", "s3cr3t-value-here", "client_secret: "},
		"json":           {`{"password": "p4ssw0rd!!", "user": "hailey"}`, "p4ssw0rd!!", `"user": "hailey"`},
		"private key":    {"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\n-----END OPENSSH PRIVATE KEY-----\nafter", "b3BlbnNzaC1rZXkt", "after"},
		"random":         {"key: Zx8Qp2Lm9Rt4Vw7Ky3Nb6Hc1Jd5Gf0Sa", "Zx8Qp2Lm9Rt4Vw7Ky3Nb6Hc1Jd5Gf0Sa", ""},
		"base64 key":     {"the key is RoO5+d7uCmDqovLrHCr2/BuViUXvdcrNxyNM1pN9dDQ= here", "RoO5+d7uCmDq", "here"},
		"numeric":        {"password=12345678", "12345678", "password="},
		"dotted":         {"token=alice.secret", "alice.secret", "token="},
		"client id":      {"client_id=dm3MEczDYODrttaXDeFi6rrNfn0qVfX52fFdS0aA", "dm3MEczDYODrtt", "client_id="},
	}
	for name, c := range cases {
		got := Text(c.in)
		if strings.Contains(got, c.gone) {
			t.Errorf("%s: %q still holds %q", name, got, c.gone)
		}
		if !strings.Contains(got, "[redacted:") {
			t.Errorf("%s: %q says nothing was redacted", name, got)
		}
		if c.kept != "" && !strings.Contains(got, c.kept) {
			t.Errorf("%s: %q lost %q", name, got, c.kept)
		}
	}
}

func TestTextKeepsWhatPeopleSearchFor(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"git checkout 6a47427b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f",
		"at://did:plc:oisofpd7lj26yvgiivf3lxsi/space/garden.engram.space/coding-agents",
		"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku",
		"session 01a16641-36cd-7695-a8b2-e41aada6d16d",
		"/root/worktrees/engram-garden/session-search/internal/agent/archive_test.go",
		"TestArchiveConcurrentPushesKeepEveryLine failed",
		"sha256:9a8b7c6d5e4f3a2b1c0d9e8f6a47427b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f",
		"the token bucket refills at 2 a second",
		"ExtractBlobsFromRecordValuesInSpace",
		"PATH=/root/go/bin:/usr/bin",
		`"maxTokens": 32000, "contextWindow": 1000000`,
		"tokensExpireAt: 1793825572",
		"--password-store=basic --use-mock-keychain",
		"if (token && Date.now() < expiresAt) return token\n    if (!refresh) {",
		"password: ${DB_PASSWORD}",
		`token := os.Getenv("GITHUB_TOKEN")`,
		"api_key: required",
		"--- FAIL: TestHandler_OIDCRedirect_UnknownHostReturns404",
		"BucketServerSideEncryptionConfigurationV2RuleApplyServerSideEncryptionByDefaultArgs",
		"ClickHouseAsyncMetrics_BlockDiscardMerges_dm_4",
		"RecordImageBatchWithResultsEvent_ToxicCheckResultsV2",
		"toolu_01Moty8hoaEtFktbDfPphcsE",
	} {
		if got := Text(s); got != s {
			t.Errorf("changed %q to %q", s, got)
		}
	}
}

func TestTextIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"ENGRAM_PASSWORD=correct-horse-battery and Bearer abcdefghijklmnop1234567890",
		"postgres://engram:hunter2hunter2@db:5432/x token ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5",
		`{"password": "p4ssw0rd!!"} key Zx8Qp2Lm9Rt4Vw7Ky3Nb6Hc1Jd5Gf0Sa`,
	} {
		once := Text(s)
		if twice := Text(once); twice != once {
			t.Errorf("redacting again changed %q to %q", once, twice)
		}
	}
}
