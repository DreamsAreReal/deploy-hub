package arch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
)

const (
	CatalogFile          = "architecture.yaml"
	TasksService         = "tasks"
	ObservabilityService = "observability"
	reasonDrift          = "the app does something its entry in policy/architecture.yaml does not declare"
	reasonNoEntry        = "the app has no entry in policy/architecture.yaml"
	reasonCatalog        = "policy/architecture.yaml cannot be read"
	dateLayout           = "2006-01-02"
	envExampleGlob       = "*.env.example"
	workflowDir          = ".github/workflows"
	caddySuffix          = ".caddy"
	timerSuffix          = ".timer"
	portSeparator        = ":"
	protoSeparator       = "/"
	wholeDay             = 24 * time.Hour
)

var (
	criticalities = []string{"high", "normal", "low"}
	gives         = []string{"logs", "metrics", "errors", "textfile", "events"}
	composeFiles  = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}
	workflowGlobs = []string{"*.yml", "*.yaml"}
	envKey        = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)
	errCatalog    = errors.New("invalid architecture catalog")
)

type Catalog struct {
	TasksCallers []string           `yaml:"tasks_callers"`
	LinkBudget   int                `yaml:"link_budget"`
	Admins       []Admin            `yaml:"admins"`
	Services     map[string]Service `yaml:"services"`
}

type Service struct {
	Owner    string      `yaml:"owner"`
	Critical string      `yaml:"critical"`
	External bool        `yaml:"external"`
	Gives    []string    `yaml:"gives"`
	Exposes  []Expose    `yaml:"exposes"`
	Calls    []Call      `yaml:"calls"`
	Timers   []Timer     `yaml:"timers"`
	Secrets  []string    `yaml:"secrets"`
	Config   []string    `yaml:"config"`
	Data     []string    `yaml:"data"`
	Isolated []Isolation `yaml:"isolated"`
}

type Expose struct {
	Site   string `yaml:"site"`
	Port   int    `yaml:"port"`
	Public bool   `yaml:"public"`
	Why    string `yaml:"why"`
}

type Call struct {
	To    string `yaml:"to"`
	Why   string `yaml:"why"`
	Until string `yaml:"until"`
	Task  string `yaml:"task"`
}

type Timer struct {
	Name string `yaml:"name"`
	Why  string `yaml:"why"`
}

type Facts struct {
	Env        []string
	Ports      []int
	Timers     []string
	Sites      []string
	Listening  []int
	Grants     []Grant
	Namespaces map[string]string
}

func Load(dir string) (Catalog, error) {
	body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, CatalogFile)))
	if err != nil {
		return Catalog{}, err
	}
	return Parse(body)
}

func Parse(body []byte) (Catalog, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("%w: %w", errCatalog, err)
	}
	if len(catalog.Services) == 0 {
		return Catalog{}, fmt.Errorf("%w: no services", errCatalog)
	}
	return catalog, nil
}

func Check(_ context.Context, ws gate.Workspace) gate.Outcome {
	catalog, err := Load(ws.PolicyDir)
	if err != nil {
		return gate.Fail(reasonCatalog, err.Error())
	}
	if _, ok := catalog.Services[ws.AppName]; !ok {
		return gate.Fail(reasonNoEntry, ws.AppName)
	}
	facts, err := Gather(ws.Src)
	if err != nil {
		return gate.Fail(reasonDrift, err.Error())
	}
	names := siblings(ws)
	HostFacts(ws.Policy.Apps, names, &facts)
	repo := catalog.Merged(names)
	findings := append(catalog.Problems(ws.AppName, time.Now()), catalog.adminProblems()...)
	findings = append(findings, repo.Drift(facts)...)
	outcome := gate.Findings(reasonDrift, findings)
	outcome.Findings = append(outcome.Findings, catalog.budgetFindings()...)
	return outcome
}

func siblings(ws gate.Workspace) []string {
	names := []string{ws.AppName}
	for name, app := range ws.Policy.Apps {
		if name != ws.AppName && app.Repo != "" && strings.EqualFold(app.Repo, ws.Repo) {
			names = append(names, name)
		}
	}
	return names
}

func (c Catalog) Merged(names []string) Service {
	var merged Service
	for _, name := range names {
		service := c.Services[name]
		merged.Secrets = append(merged.Secrets, service.Secrets...)
		merged.Config = append(merged.Config, service.Config...)
		merged.Exposes = append(merged.Exposes, service.Exposes...)
		merged.Timers = append(merged.Timers, service.Timers...)
	}
	return merged
}

func (c Catalog) Problems(name string, now time.Time) []string {
	service := c.Services[name]
	var problems []string
	if !slices.Contains(criticalities, service.Critical) {
		problems = append(problems, fmt.Sprintf("%s: critical must be one of %v", name, criticalities))
	}
	for _, give := range service.Gives {
		if !slices.Contains(gives, give) {
			problems = append(problems, fmt.Sprintf("%s: gives %q is not one of %v", name, give, gives))
		}
	}
	for _, call := range service.Calls {
		problems = append(problems, c.callProblems(name, call, now)...)
	}
	for _, timer := range service.Timers {
		if strings.TrimSpace(timer.Why) == "" {
			problems = append(problems, fmt.Sprintf("%s: timer %q without a why: a schedule needs a reason it cannot wait for an event", name, timer.Name))
		}
	}
	return append(problems, service.hostProblems(name)...)
}

func (c Catalog) callProblems(name string, call Call, now time.Time) []string {
	var problems []string
	if _, known := c.Services[call.To]; !known {
		problems = append(problems, fmt.Sprintf("%s calls %q, which is not in the catalog", name, call.To))
	}
	if strings.TrimSpace(call.Why) == "" {
		problems = append(problems, fmt.Sprintf("%s calls %s without a why: a new link needs a reason it cannot go through logs or metrics", name, call.To))
	}
	if call.To == TasksService && !slices.Contains(c.TasksCallers, name) {
		problems = append(problems, exception(name, call, now)...)
	}
	return problems
}

func exception(name string, call Call, now time.Time) []string {
	until, err := time.Parse(dateLayout, call.Until)
	switch {
	case call.Until == "" || call.Task == "":
		return []string{fmt.Sprintf("%s calls tasks directly: only tasks_callers may, others need until and task of the fix", name)}
	case err != nil:
		return []string{fmt.Sprintf("%s: until %q is not a date like 2026-12-31", name, call.Until)}
	case now.After(until.Add(wholeDay)):
		return []string{fmt.Sprintf("%s: the exception to call tasks ended on %s (%s)", name, call.Until, call.Task)}
	}
	return nil
}

const (
	networkShared = "docker network %s also connects %s, and the catalog has no call between them"
	envInRepo     = "environment variable %s is in the repo but not in secrets or config"
	portInRepo    = "port %d is published by compose but not in exposes"
	envRunning    = "environment variable %s is set in the running container but not in secrets or config"
	portRunning   = "port %d is published by the running container but not in exposes"
	timerInRepo   = "%s runs on a schedule but is not in timers"
	siteInRepo    = "public site %s comes from a host caddy file but is not in exposes"
	siteRunning   = "public site %s is served by the hub (app.conf has no site=off) but is not in exposes"
	timerOnHost   = "timer %s is installed on the host but no service declares it in timers"
	siteOnHost    = "public site %s has a caddy file on the host but no service declares it in exposes"
)

func (s Service) Drift(facts Facts) []string {
	drift := s.drift(facts, envInRepo, portInRepo)
	drift = append(drift, undeclared(facts.Timers, s.timerNames(), timerInRepo)...)
	return append(drift, undeclared(facts.Sites, s.siteNames(), siteInRepo)...)
}

func (s Service) LiveDrift(facts Facts) []string {
	drift := s.drift(facts, envRunning, portRunning)
	return append(drift, undeclared(facts.Sites, s.siteNames(), siteRunning)...)
}

func (c Catalog) HostDrift(facts Facts) []string {
	var timers, sites []string
	for _, service := range c.Services {
		timers = append(timers, service.timerNames()...)
		sites = append(sites, service.siteNames()...)
	}
	drift := undeclared(facts.Timers, timers, timerOnHost)
	drift = append(drift, undeclared(facts.Sites, sites, siteOnHost)...)
	drift = append(drift, c.listenDrift(facts.Listening)...)
	drift = append(drift, c.adminDrift(facts.Grants)...)
	return append(drift, c.netnsDrift(facts.Namespaces)...)
}

func Installed(unitDir, siteDir string) (Facts, error) {
	timers, err := hostFiles(unitDir, timerSuffix)
	if err != nil {
		return Facts{}, err
	}
	sites, err := hostFiles(siteDir, caddySuffix)
	for i, site := range sites {
		sites[i] = strings.TrimSuffix(site, caddySuffix)
	}
	return Facts{Timers: timers, Sites: sites}, err
}

func hostFiles(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, suffix) && !strings.HasPrefix(name, ".") && active(dir, entry) {
			names = append(names, name)
		}
	}
	return names, err
}

func active(dir string, entry os.DirEntry) bool {
	if entry.Type()&os.ModeSymlink == 0 {
		return entry.Type().IsRegular()
	}
	target, err := os.Readlink(filepath.Join(dir, entry.Name()))
	return err == nil && target != os.DevNull
}

func (s Service) drift(facts Facts, envLayout, portLayout string) []string {
	drift := undeclared(facts.Env, append(slices.Clone(s.Secrets), s.Config...), envLayout)
	for _, port := range facts.Ports {
		if !s.exposes(port) {
			drift = append(drift, fmt.Sprintf(portLayout, port))
		}
	}
	return drift
}

func undeclared(found, declared []string, layout string) []string {
	var out []string
	for _, name := range found {
		if !slices.Contains(declared, name) {
			out = append(out, fmt.Sprintf(layout, name))
		}
	}
	return out
}

func (s Service) timerNames() []string {
	names := make([]string, 0, len(s.Timers))
	for _, timer := range s.Timers {
		names = append(names, timer.Name)
	}
	return names
}

func (s Service) siteNames() []string {
	var names []string
	for _, expose := range s.Exposes {
		if expose.Site != "" {
			names = append(names, expose.Site)
		}
	}
	return names
}

func (c Catalog) Linked(a, b string) bool {
	return c.calls(a, b) || c.calls(b, a) || c.collects(a, b) || c.collects(b, a)
}

func (c Catalog) calls(from, to string) bool {
	return slices.ContainsFunc(c.Services[from].Calls, func(call Call) bool { return call.To == to })
}

func (c Catalog) collects(collector, service string) bool {
	return collector == ObservabilityService && len(c.Services[service].Gives) > 0
}

func (c Catalog) NetworkDrift(networks map[string][]string) map[string][]string {
	members := map[string][]string{}
	for _, app := range slices.Sorted(maps.Keys(networks)) {
		for _, network := range networks[app] {
			members[network] = append(members[network], app)
		}
	}
	drift := map[string][]string{}
	for _, network := range slices.Sorted(maps.Keys(members)) {
		for _, pair := range pairs(members[network]) {
			if !c.Linked(pair[0], pair[1]) {
				drift[pair[0]] = append(drift[pair[0]], fmt.Sprintf(networkShared, network, pair[1]))
				drift[pair[1]] = append(drift[pair[1]], fmt.Sprintf(networkShared, network, pair[0]))
			}
		}
	}
	return drift
}

func pairs(apps []string) [][2]string {
	var out [][2]string
	for i, a := range apps {
		for _, b := range apps[i+1:] {
			out = append(out, [2]string{a, b})
		}
	}
	return out
}

func (s Service) exposes(port int) bool {
	for _, expose := range s.Exposes {
		if expose.Port == port {
			return true
		}
	}
	return false
}

func Gather(src string) (Facts, error) {
	var facts Facts
	examples, err := filepath.Glob(filepath.Join(src, envExampleGlob))
	if err != nil {
		return facts, err
	}
	for _, path := range examples {
		keys, err := envKeys(path)
		if err != nil {
			return facts, err
		}
		facts.Env = appendNew(facts.Env, keys...)
	}
	if err := composeFacts(src, &facts); err != nil {
		return facts, err
	}
	return facts, workflowFacts(src, &facts)
}

func HostFacts(apps map[string]policy.App, names []string, facts *Facts) {
	for _, name := range names {
		host := apps[name].Host
		if host == nil {
			continue
		}
		facts.Timers = appendNew(facts.Timers, host.Timers...)
		for _, file := range host.Files {
			if path.Dir(file.Dst)+"/" == policy.CaddySiteDir {
				facts.Sites = appendNew(facts.Sites, strings.TrimSuffix(path.Base(file.Dst), caddySuffix))
			}
		}
	}
}

type workflow struct {
	On any `yaml:"on"`
}

func workflowFacts(src string, facts *Facts) error {
	var files []string
	for _, glob := range workflowGlobs {
		found, err := filepath.Glob(filepath.Join(src, workflowDir, glob))
		if err != nil {
			return err
		}
		files = append(files, found...)
	}
	for _, file := range files {
		scheduled, err := isScheduled(file)
		if err != nil {
			return err
		}
		if scheduled {
			facts.Timers = appendNew(facts.Timers, path.Join(workflowDir, filepath.Base(file)))
		}
	}
	return nil
}

func isScheduled(file string) (bool, error) {
	body, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return false, err
	}
	var parsed workflow
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		return false, fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	triggers, ok := parsed.On.(map[string]any)
	if !ok {
		return false, nil
	}
	_, scheduled := triggers["schedule"]
	return scheduled, nil
}

func envKeys(path string) ([]string, error) {
	body, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var keys []string
	for _, line := range strings.Split(string(body), "\n") {
		if match := envKey.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			keys = append(keys, match[1])
		}
	}
	return keys, err
}

func appendNew(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

type compose struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Ports       []string `yaml:"ports"`
	Environment any      `yaml:"environment"`
}

func composeFacts(src string, facts *Facts) error {
	for _, name := range composeFiles {
		body, err := os.ReadFile(filepath.Clean(filepath.Join(src, name)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var parsed compose
		if err := yaml.Unmarshal(body, &parsed); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		parsed.collect(facts)
	}
	return nil
}

func (c compose) collect(facts *Facts) {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		service := c.Services[name]
		for _, mapping := range service.Ports {
			if port, ok := containerPort(mapping); ok && !slices.Contains(facts.Ports, port) {
				facts.Ports = append(facts.Ports, port)
			}
		}
		facts.Env = appendNew(facts.Env, environmentKeys(service.Environment)...)
	}
}

func containerPort(mapping string) (int, bool) {
	parts := strings.Split(mapping, portSeparator)
	last, _, _ := strings.Cut(parts[len(parts)-1], protoSeparator)
	port, err := strconv.Atoi(last)
	return port, err == nil
}

func environmentKeys(environment any) []string {
	var keys []string
	switch typed := environment.(type) {
	case map[string]any:
		for key := range typed {
			keys = append(keys, key)
		}
	case []any:
		for _, entry := range typed {
			key, _, _ := strings.Cut(fmt.Sprint(entry), "=")
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (c Catalog) Mermaid() string {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	lines := []string{"graph LR"}
	for _, name := range names {
		for _, call := range c.Services[name].Calls {
			lines = append(lines, fmt.Sprintf("  %s -->|%s| %s", node(name), label(call), node(call.To)))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func node(name string) string {
	return strings.ReplaceAll(name, "-", "_") + "[" + name + "]"
}

func label(call Call) string {
	text := strings.ReplaceAll(call.Why, "|", "/")
	if call.Until != "" {
		text += " (until " + call.Until + ", " + call.Task + ")"
	}
	return text
}
