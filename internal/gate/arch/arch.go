package arch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	CatalogFile    = "architecture.yaml"
	TasksService   = "tasks"
	reasonDrift    = "the app does something its entry in policy/architecture.yaml does not declare"
	reasonNoEntry  = "the app has no entry in policy/architecture.yaml"
	reasonCatalog  = "policy/architecture.yaml cannot be read"
	dateLayout     = "2006-01-02"
	envExampleGlob = "*.env.example"
	portSeparator  = ":"
	protoSeparator = "/"
	wholeDay       = 24 * time.Hour
)

var (
	criticalities = []string{"high", "normal", "low"}
	gives         = []string{"logs", "metrics", "errors", "textfile", "events"}
	composeFiles  = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}
	envKey        = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)
	errCatalog    = errors.New("invalid architecture catalog")
)

type Catalog struct {
	TasksCallers []string           `yaml:"tasks_callers"`
	LinkBudget   int                `yaml:"link_budget"`
	Services     map[string]Service `yaml:"services"`
}

type Service struct {
	Owner    string   `yaml:"owner"`
	Critical string   `yaml:"critical"`
	External bool     `yaml:"external"`
	Gives    []string `yaml:"gives"`
	Exposes  []Expose `yaml:"exposes"`
	Calls    []Call   `yaml:"calls"`
	Secrets  []string `yaml:"secrets"`
	Config   []string `yaml:"config"`
	Data     []string `yaml:"data"`
}

type Expose struct {
	Site string `yaml:"site"`
	Port int    `yaml:"port"`
}

type Call struct {
	To    string `yaml:"to"`
	Why   string `yaml:"why"`
	Until string `yaml:"until"`
	Task  string `yaml:"task"`
}

type Facts struct {
	Env   []string
	Ports []int
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
	repo := catalog.Merged(siblings(ws))
	findings := append(catalog.Problems(ws.AppName, time.Now()), repo.Drift(facts)...)
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
	return problems
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
	envInRepo   = "environment variable %s is in the repo but not in secrets or config"
	portInRepo  = "port %d is published by compose but not in exposes"
	envRunning  = "environment variable %s is set in the running container but not in secrets or config"
	portRunning = "port %d is published by the running container but not in exposes"
)

func (s Service) Drift(facts Facts) []string {
	return s.drift(facts, envInRepo, portInRepo)
}

func (s Service) LiveDrift(facts Facts) []string {
	return s.drift(facts, envRunning, portRunning)
}

func (s Service) drift(facts Facts, envLayout, portLayout string) []string {
	declared := append(slices.Clone(s.Secrets), s.Config...)
	var drift []string
	for _, key := range facts.Env {
		if !slices.Contains(declared, key) {
			drift = append(drift, fmt.Sprintf(envLayout, key))
		}
	}
	for _, port := range facts.Ports {
		if !s.exposes(port) {
			drift = append(drift, fmt.Sprintf(portLayout, port))
		}
	}
	return drift
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
	return facts, composeFacts(src, &facts)
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
