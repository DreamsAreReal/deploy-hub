package onboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/compose"
	"github.com/DreamsAreReal/deploy-hub/internal/provision"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	firstAppPort   = 9001
	staticCPort    = "80"
	serviceCPort   = "8080"
	staticMem      = "64m"
	serviceMem     = "256m"
	botMem         = "128m"
	stubPath       = ".github/workflows/deploy.yml"
	appStubPath    = ".github/workflows/deploy-%s.yml"
	stubName       = "deploy"
	hubUsesPattern = "uses: %s/"
)

var (
	exposeLine   = regexp.MustCompile(`(?im)^\s*EXPOSE\s+([0-9]+)`)
	envExample   = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)=`)
	composeVar   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)[:}?-]`)
	composeNames = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml"}
	envExamples  = []string{".env.example", ".env.sample", "env.example"}
	errMulti     = errors.New(errMultiBuild)
)

func (o Onboarder) gather(ctx context.Context, s *session) error {
	steps := []func(context.Context, *session) error{
		o.resolveBranch, o.clone, o.inspectRepo, o.serverFacts, o.choosePort, o.inspectStub, o.listSecrets,
	}
	for _, step := range steps {
		if err := step(ctx, s); err != nil {
			return err
		}
	}
	s.envVars = envVars(s.repoPath(), s.composeIn)
	return nil
}

func (s *session) repoPath(parts ...string) string {
	return filepath.Join(append([]string{s.work, repoDir}, parts...)...)
}

func (o Onboarder) resolveBranch(ctx context.Context, s *session) error {
	branch, err := o.run(ctx, ghBinary, "repo", "view", s.opts.Repo, "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name")
	if err != nil || branch == "" {
		return fmt.Errorf(errNoRepo, s.opts.Repo)
	}
	s.branch = branch
	owner, _, _ := strings.Cut(s.opts.Repo, slugSeparator)
	s.image = imagePrefix + strings.ToLower(owner) + slugSeparator + s.opts.App
	return nil
}

func (o Onboarder) clone(ctx context.Context, s *session) error {
	_, err := o.run(ctx, gitBinary, "clone", "-q", "--depth", "1", "-b", s.branch, gitHubPrefix+s.opts.Repo+gitSuffix, s.repoPath())
	return err
}

func (o Onboarder) inspectRepo(_ context.Context, s *session) error {
	dockerfile, err := os.ReadFile(s.repoPath(s.opts.Dockerfile))
	if err != nil {
		return fmt.Errorf(errNoDockerfile, s.opts.Dockerfile)
	}
	if err := s.inspectCompose(); err != nil {
		return err
	}
	if s.opts.Profile == "" {
		o.detectProfile(s, dockerfile)
	}
	applyProfileDefaults(&s.opts)
	return nil
}

func (s *session) inspectCompose() error {
	if s.opts.Compose == "" {
		s.composeIn, _ = readFirst(s.repoPath(), composeNames)
	} else if data, err := os.ReadFile(s.repoPath(s.opts.Compose)); err == nil {
		s.composeIn = data
	} else {
		return fmt.Errorf(errNoCompose, s.opts.Compose)
	}
	if s.composeIn == nil {
		return nil
	}
	sum, err := compose.Summarize(s.composeIn)
	if err == nil && sum.Built > 1 {
		return errMulti
	}
	if err == nil {
		s.compose = &sum
	}
	return nil
}

func readFirst(dir string, names []string) ([]byte, bool) {
	for _, n := range names {
		if data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, n))); err == nil {
			return data, true
		}
	}
	return nil, false
}

func (o Onboarder) detectProfile(s *session, dockerfile []byte) {
	cport, note := "", detectBot
	s.opts.Profile = registry.ProfileBot
	if m := exposeLine.FindSubmatch(dockerfile); m != nil {
		s.opts.Profile, cport, note = registry.ProfileService, string(m[1]), fmt.Sprintf(detectExpose, m[1])
	} else if s.compose != nil && s.compose.ContainerPort != "" {
		s.opts.Profile, cport, note = registry.ProfileService, s.compose.ContainerPort, detectCompose
	}
	if s.opts.CPort == "" {
		s.opts.CPort = cport
	}
	o.Out.Printf(msgDetected, note)
}

func applyProfileDefaults(o *Options) {
	defaults := map[string][2]string{
		registry.ProfileStatic:  {staticCPort, staticMem},
		registry.ProfileService: {serviceCPort, serviceMem},
		registry.ProfileBot:     {"", botMem},
	}
	d := defaults[o.Profile]
	if o.Profile == registry.ProfileBot {
		o.CPort, o.Port = "", ""
	}
	if o.CPort == "" {
		o.CPort = d[0]
	}
	if o.Mem == "" {
		o.Mem = d[1]
	}
}

func (o Onboarder) serverFacts(ctx context.Context, s *session) error {
	out, err := o.remote(ctx, "facts", s.opts.App)
	if err != nil {
		return err
	}
	s.server = provision.ParseFacts(out)
	return nil
}

func (o Onboarder) choosePort(_ context.Context, s *session) error {
	port, err := pickPort(s.opts.Profile, s.opts.Port, s.server.Port, s.server.PortsTaken)
	s.opts.Port = port
	return err
}

func pickPort(profile, requested, existing string, taken []int) (string, error) {
	switch {
	case profile == registry.ProfileBot:
		return "", nil
	case existing != "":
		return existing, nil
	case requested == "":
		p := firstAppPort
		for slices.Contains(taken, p) {
			p++
		}
		return strconv.Itoa(p), nil
	}
	n, _ := strconv.Atoi(requested)
	if slices.Contains(taken, n) {
		return "", fmt.Errorf(errPortTaken, requested)
	}
	return requested, nil
}

func StubPath(opts Options) string {
	if opts.Compose == "" {
		return stubPath
	}
	return fmt.Sprintf(appStubPath, opts.App)
}

func Stub(branch, hubRepo string, opts Options) string {
	name, input := stubName, ""
	if opts.Dockerfile != defaultDockerfile {
		input = fmt.Sprintf(stubDockerfileInput, opts.Dockerfile)
	}
	if opts.Compose != "" {
		name = stubName + "-" + opts.App
		input += fmt.Sprintf(stubComposeInput, opts.Compose)
	}
	return fmt.Sprintf(stubTemplate, name, branch, hubRepo, opts.App, input)
}

func (o Onboarder) inspectStub(_ context.Context, s *session) error {
	s.stubPath = StubPath(s.opts)
	s.stubWant = Stub(s.branch, o.Config.HubRepo, s.opts)
	current, err := os.ReadFile(s.repoPath(s.stubPath))
	switch {
	case err != nil:
		s.stubState = stubStateMissing
	case strings.TrimRight(string(current), "\n") == s.stubWant:
		s.stubState = stubStateOK
	case strings.Contains(string(current), fmt.Sprintf(hubUsesPattern, o.Config.HubRepo)):
		s.stubState = stubStateDiffers
	default:
		return fmt.Errorf(errOwnCI, s.opts.Repo)
	}
	return nil
}

func (o Onboarder) listSecrets(ctx context.Context, s *session) error {
	out, _ := o.run(ctx, ghBinary, "secret", "list", "-R", s.opts.Repo, "--json", "name", "-q", ".[].name")
	s.secrets = map[string]bool{}
	for _, name := range strings.Fields(out) {
		s.secrets[name] = true
	}
	return nil
}

func envVars(repo string, composeIn []byte) []string {
	if data, ok := readFirst(repo, envExamples); ok {
		return uniqueMatches(envExample, data)
	}
	if composeIn != nil {
		vars := uniqueMatches(composeVar, composeIn)
		slices.Sort(vars)
		return vars
	}
	return nil
}

func uniqueMatches(re *regexp.Regexp, data []byte) []string {
	var out []string
	for _, m := range re.FindAllSubmatch(data, -1) {
		if v := string(m[1]); !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
