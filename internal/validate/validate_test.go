package validate

import (
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const withSpace = "a b"

type validatorCase struct {
	name string
	fn   func(string) error
	ok   []string
	bad  []string
}

func (c validatorCase) run(t *testing.T) {
	t.Helper()
	for _, v := range c.ok {
		testutil.NoErr(t, c.fn(v))
	}
	for _, v := range c.bad {
		testutil.Is(t, c.fn(v), ErrInvalid)
	}
}

func TestValidators(t *testing.T) {
	cases := []validatorCase{
		{"app", AppName, []string{"a", "my-app_1.x", "0app"}, []string{"", "-app", "App", withSpace, "a;rm", "../etc", "a/b", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnop"}},
		{"tag", Tag, []string{"sha-abcdef0", "sha-" + "0123456789abcdef0123456789abcdef01234567"}, []string{"", "sha-abc", "sha-ABCDEF0", "latest", "sha-abcdef0;x", "sha-" + "0123456789abcdef0123456789abcdef012345678"}},
		{"port", Port, []string{"80", "9001", "65535"}, []string{"", "8", "65536", "99999", "80a", "-80", "123456"}},
		{"mem", MemLimit, []string{"64m", "1g", "12345m"}, []string{"", "64", "64k", "123456m", "m"}},
		{"health", HealthPath, []string{"/", "/healthz", "/a/b.c-d_e"}, []string{"", "healthz", "/a b", "/a?b", "/$(id)"}},
		{"image", Image, []string{"ghcr.io/owner/app"}, []string{"", "docker.io/owner/app", "ghcr.io/", "ghcr.io/Owner/App", "ghcr.io/a b"}},
		{"service", Service, []string{"web", "Web.1_x-y"}, []string{"", withSpace, "a;b"}},
		{"slug", RepoSlug, []string{"Owner/repo", "a/b.c_d-e"}, []string{"", "repo", "a/b/c", "a b/c", "/repo"}},
		{"start", StartEpoch, []string{"0", "1700000000"}, []string{"", "-1", "1e9", "1234567890123"}},
		{"relpath", RelPath, []string{"Dockerfile", "docker/Dockerfile", "./a/b"}, []string{"", "/etc/passwd", "..", "../x", "a/../../x", withSpace}},
	}
	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}

func TestOptionalPort(t *testing.T) {
	testutil.NoErr(t, OptionalPort(""))
	testutil.NoErr(t, OptionalPort("8080"))
	testutil.Is(t, OptionalPort("x"), ErrInvalid)
}
