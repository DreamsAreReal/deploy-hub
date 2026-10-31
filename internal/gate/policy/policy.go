package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	KindBinary  = "binary"
	KindService = "service"
	KindWorker  = "worker"

	NotALicense = "NONE"
	exprAnd     = "AND"
	exprOr      = "OR"

	CategorySecurity = "security"
	CategoryBusiness = "business"

	percentCeiling    = 100
	maxParallelChecks = 16
	purlScheme        = "pkg:"
	policyChecks      = 9
	appChecks         = 6
)

var (
	cpus      = regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,2})?$`)
	digestRef = regexp.MustCompile(`^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$`)
	taskRef   = regexp.MustCompile(`#[0-9]+`)
)

var licenseDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var (
	ErrPolicy     = errors.New("invalid gate policy")
	ErrUnknownApp = errors.New("app is not registered in the gate policy")
)

type Policy struct {
	Owners         []string          `yaml:"owners"`
	Thresholds     Thresholds        `yaml:"thresholds"`
	Limits         Limits            `yaml:"limits"`
	Licenses       []string          `yaml:"licenses"`
	Dependencies   []string          `yaml:"dependencies"`
	Declared       map[string]string `yaml:"declared_licenses"`
	ReviewedTexts  map[string]string `yaml:"reviewed_license_texts"`
	ForbiddenFiles []string          `yaml:"forbidden_files"`
	Markers        []string          `yaml:"markers"`
	SASTRules      []string          `yaml:"sast_rulesets"`
	Images         Images            `yaml:"images"`
	Apps           map[string]App    `yaml:"apps"`
	Hub            Hub               `yaml:"hub"`
	Mutation       Mutation          `yaml:"mutation"`
}

type Mutation struct {
	SkipInDeploys bool `yaml:"skip_in_deploys"`
}

type Images struct {
	Semgrep string `yaml:"semgrep"`
	ZAP     string `yaml:"zap"`
	Browser string `yaml:"browser"`
}

type Thresholds struct {
	LineCoverage    float64 `yaml:"line_coverage"`
	BranchCoverage  float64 `yaml:"branch_coverage"`
	MutationScore   float64 `yaml:"mutation_score"`
	MaxCognitive    int     `yaml:"max_cognitive_complexity"`
	MaxDuplication  float64 `yaml:"max_duplication_percent"`
	DuplicateTokens int     `yaml:"duplicate_min_tokens"`
	MaxNetGrowth    int     `yaml:"max_net_growth_lines"`
}

type Limits struct {
	Check            time.Duration `yaml:"check_timeout"`
	LongCheck        time.Duration `yaml:"long_check_timeout"`
	FuzzTime         time.Duration `yaml:"fuzz_time"`
	FuzzExamples     int           `yaml:"fuzz_examples"`
	MutantFloor      time.Duration `yaml:"mutant_timeout_floor"`
	MutantFactor     int           `yaml:"mutant_timeout_factor"`
	FindingsListed   int           `yaml:"findings_listed"`
	StageReady       time.Duration `yaml:"stage_ready_timeout"`
	MutationDisk     int64         `yaml:"mutation_disk_gib"`
	MutationFree     int64         `yaml:"mutation_free_gib"`
	ParallelChecks   int           `yaml:"parallel_checks"`
	ParallelPackages int           `yaml:"parallel_packages"`
}

type App struct {
	Repo      string      `yaml:"repo"`
	Kind      string      `yaml:"kind"`
	Port      int         `yaml:"port"`
	Health    string      `yaml:"health"`
	CPUs      string      `yaml:"cpus"`
	Memory    string      `yaml:"memory"`
	DAST      DAST        `yaml:"dast"`
	Env       []string    `yaml:"env"`
	Data      []string    `yaml:"data"`
	Scenarios []Scenario  `yaml:"scenarios"`
	Load      LoadProfile `yaml:"load"`
	Localized []string    `yaml:"localized"`
	Allowed   []string    `yaml:"allowed_files"`
	Accepted  []Accepted  `yaml:"accepted_workflow_findings"`
	Host      *Host       `yaml:"host"`
	Shell     string      `yaml:"shell_utilities"`
	UI        *UI         `yaml:"ui"`
}

type UI struct {
	Map      string   `yaml:"map"`
	Seed     []string `yaml:"seed"`
	Env      []string `yaml:"env"`
	Chromium string   `yaml:"chromium"`
}

const ShellBusybox = "busybox"

type Accepted struct {
	Rule     string `yaml:"rule"`
	Severity string `yaml:"severity"`
	File     string `yaml:"file"`
	Key      string `yaml:"key"`
	Reason   string `yaml:"reason"`
}

type Scenario struct {
	ID       string   `yaml:"id"`
	Category string   `yaml:"category"`
	Tests    []string `yaml:"tests"`
}

type LoadProfile struct {
	Rate        int           `yaml:"rate"`
	Duration    time.Duration `yaml:"duration"`
	P95         time.Duration `yaml:"p95"`
	MaxErrors   float64       `yaml:"max_error_percent"`
	Concurrency int           `yaml:"concurrency"`
	Paths       []string      `yaml:"paths"`
}

type DAST struct {
	Target  string        `yaml:"target"`
	OpenAPI string        `yaml:"openapi"`
	Spider  time.Duration `yaml:"spider"`
	MaxTime time.Duration `yaml:"max_time"`
}

func LoadFile(path string) (Policy, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Policy{}, err
	}
	return Parse(data)
}

func Parse(data []byte) (Policy, error) {
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrPolicy, err)
	}
	return p, p.Validate()
}

func (p Policy) App(name string) (App, error) {
	app, ok := p.Apps[name]
	if !ok {
		return App{}, fmt.Errorf("%w: %s", ErrUnknownApp, name)
	}
	return app, nil
}

func (p Policy) Owns(owner string) bool {
	for _, o := range p.Owners {
		if o == owner {
			return true
		}
	}
	return false
}

func (p Policy) Validate() error {
	errs := make([]error, 0, len(p.Apps)+policyChecks)
	errs = append(errs,
		p.Thresholds.validate(), p.Limits.validate(), p.Images.validate(),
		nonEmpty("owners", p.Owners), nonEmpty("licenses", p.Licenses), nonEmpty("markers", p.Markers), nonEmpty("sast_rulesets", p.SASTRules),
		p.validateDeclared(), p.validateReviewedTexts(), p.Hub.validate(), p.validateBrowser(),
	)
	for name, app := range p.Apps {
		errs = append(errs, app.validate(name))
	}
	return errors.Join(errs...)
}

func (p Policy) validateDeclared() error {
	var errs []error
	for purl, license := range p.Declared {
		if !strings.HasPrefix(purl, purlScheme) || strings.Contains(purl, "@") {
			errs = append(errs, fmt.Errorf("%w: declared_licenses key %q must be a package URL without a version", ErrPolicy, purl))
		}
		if !slices.Contains(p.Licenses, license) {
			errs = append(errs, fmt.Errorf("%w: declared license %s of %s is not an approved license", ErrPolicy, license, purl))
		}
	}
	return errors.Join(errs...)
}

func (p Policy) validateReviewedTexts() error {
	var errs []error
	for digest, expr := range p.ReviewedTexts {
		if !licenseDigest.MatchString(digest) {
			errs = append(errs, fmt.Errorf("%w: reviewed_license_texts key %q must be sha256:<64 hex>", ErrPolicy, digest))
		}
		if expr != NotALicense && !p.approvedExpression(expr) {
			errs = append(errs, fmt.Errorf("%w: reviewed license text %s maps to %q, which is not built from approved licenses", ErrPolicy, digest, expr))
		}
	}
	return errors.Join(errs...)
}

func (p Policy) approvedExpression(expr string) bool {
	ids := strings.FieldsFunc(expr, func(r rune) bool { return r == '(' || r == ')' })
	terms := strings.Fields(strings.Join(ids, " "))
	for i, term := range terms {
		operator := term == exprAnd || term == exprOr
		if operator == (i%2 == 0) || (!operator && !slices.Contains(p.Licenses, term)) {
			return false
		}
	}
	return len(terms)%2 == 1
}

func nonEmpty(field string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%w: %s must not be empty", ErrPolicy, field)
	}
	return nil
}

func percent(field string, v float64) error {
	if v <= 0 || v > percentCeiling {
		return fmt.Errorf("%w: %s must be in (0, 100]", ErrPolicy, field)
	}
	return nil
}

func notNegative(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("%w: %s must not be negative", ErrPolicy, field)
	}
	return nil
}

func positive(field string, v int64) error {
	if v <= 0 {
		return fmt.Errorf("%w: %s must be positive", ErrPolicy, field)
	}
	return nil
}

func (t Thresholds) validate() error {
	return errors.Join(
		percent("line_coverage", t.LineCoverage),
		percent("branch_coverage", t.BranchCoverage),
		percent("mutation_score", t.MutationScore),
		percent("max_duplication_percent", t.MaxDuplication),
		positive("max_cognitive_complexity", int64(t.MaxCognitive)),
		positive("duplicate_min_tokens", int64(t.DuplicateTokens)),
		notNegative("max_net_growth_lines", int64(t.MaxNetGrowth)),
	)
}

func (l Limits) validate() error {
	return errors.Join(
		positive("check_timeout", int64(l.Check)),
		positive("long_check_timeout", int64(l.LongCheck)),
		positive("fuzz_time", int64(l.FuzzTime)),
		positive("fuzz_examples", int64(l.FuzzExamples)),
		positive("mutant_timeout_floor", int64(l.MutantFloor)),
		positive("mutant_timeout_factor", int64(l.MutantFactor)),
		positive("findings_listed", int64(l.FindingsListed)),
		positive("stage_ready_timeout", int64(l.StageReady)),
		positive("mutation_disk_gib", l.MutationDisk),
		positive("mutation_free_gib", l.MutationFree),
		atMost("parallel_checks", int64(l.ParallelChecks), maxParallelChecks),
		atMost("parallel_packages", int64(l.ParallelPackages), maxParallelChecks),
	)
}

func atMost(field string, v, limit int64) error {
	if v < 0 || v > limit {
		return fmt.Errorf("%w: %s must be in [0, %d]", ErrPolicy, field, limit)
	}
	return nil
}

func (i Images) validate() error {
	return errors.Join(pinned("images.semgrep", i.Semgrep), pinned("images.zap", i.ZAP))
}

func (p Policy) validateBrowser() error {
	for _, app := range p.Apps {
		if app.UI != nil {
			return pinned("images.browser", p.Images.Browser)
		}
	}
	return nil
}

func pinned(field, ref string) error {
	if !digestRef.MatchString(ref) {
		return fmt.Errorf("%w: %s must be pinned by sha256 digest", ErrPolicy, field)
	}
	return nil
}

func (a App) validate(name string) error {
	errs := make([]error, 0, len(a.Scenarios)+appChecks)
	errs = append(errs, validate.AppName(name), validate.RepoSlug(a.Repo), a.validateKind(name), validateHost(name, a.Host), a.validateShell(name), a.UI.validate(name))
	seen := map[string]bool{}
	for _, s := range a.Scenarios {
		errs = append(errs, s.validate(name, seen))
	}
	for _, p := range a.Localized {
		errs = append(errs, validPattern(name, "localized", p))
	}
	for _, p := range a.Allowed {
		errs = append(errs, validPattern(name, "allowed_files", p))
	}
	for _, f := range a.Accepted {
		errs = append(errs, f.validate(name))
	}
	return errors.Join(errs...)
}

func (u *UI) validate(name string) error {
	if u == nil || (u.Map != "" && !path.IsAbs(u.Map) && path.Clean(u.Map) == u.Map && !strings.HasPrefix(u.Map, "..") && path.IsAbs(u.Chromium)) {
		return nil
	}
	return fmt.Errorf("%w: %s: ui needs the repo path of its app map and the absolute path of chromium in the browser image", ErrPolicy, name)
}

func (a App) validateShell(name string) error {
	if a.Shell != "" && a.Shell != ShellBusybox {
		return fmt.Errorf("%w: %s: shell_utilities must be empty or %q", ErrPolicy, name, ShellBusybox)
	}
	return nil
}

func (f Accepted) validate(app string) error {
	if f.Rule == "" || f.Severity == "" || f.Key == "" || !taskRef.MatchString(f.Reason) ||
		strings.ContainsAny(f.File, "*?[") || validPattern(app, "accepted_workflow_findings", f.File) != nil {
		return fmt.Errorf("%w: app %s: accepted workflow finding %q needs a rule, a severity, an exact relative file, a key and a reason that cites a task (#N)", ErrPolicy, app, f.Rule)
	}
	return nil
}

func validPattern(app, field, pattern string) error {
	_, err := path.Match(pattern, "")
	if err != nil || pattern == "" || path.IsAbs(pattern) || slices.Contains(strings.Split(pattern, "/"), "..") {
		return fmt.Errorf("%w: app %s: %s pattern %q must be a relative path pattern", ErrPolicy, app, field, pattern)
	}
	return nil
}

func (a App) IsLocalized(rel string) bool {
	return slices.ContainsFunc(a.Localized, func(p string) bool { return pathMatch(p, rel) })
}

func (a App) AllowsFile(rel string) bool {
	return slices.ContainsFunc(a.Allowed, func(p string) bool { return pathMatch(p, rel) })
}

func pathMatch(pattern, rel string) bool {
	switch {
	case strings.HasSuffix(pattern, "/"):
		return strings.HasPrefix(rel, pattern) || strings.Contains(rel, "/"+pattern)
	case strings.Contains(pattern, "/"):
		ok, _ := path.Match(pattern, rel)
		return ok
	default:
		ok, _ := path.Match(pattern, path.Base(rel))
		return ok
	}
}

func (a App) validateKind(name string) error {
	switch a.Kind {
	case KindBinary, KindWorker:
		return nil
	case KindService:
		return errors.Join(
			validate.Port(fmt.Sprint(a.Port)), validate.HealthPath(a.Health), validate.MemLimit(a.Memory),
			validCPUs("apps."+name+".cpus", a.CPUs), a.Load.validate(name), a.DAST.validate(name), validData(name, a.Data),
		)
	default:
		return fmt.Errorf("%w: app %s: unknown kind %q", ErrPolicy, name, a.Kind)
	}
}

func (l LoadProfile) validate(app string) error {
	field := "apps." + app + ".load."
	return errors.Join(
		positive(field+"rate", int64(l.Rate)),
		positive(field+"duration", int64(l.Duration)),
		positive(field+"p95", int64(l.P95)),
		positive(field+"concurrency", int64(l.Concurrency)),
		validPaths(field+"paths", l.Paths),
	)
}

func validPaths(field string, paths []string) error {
	errs := make([]error, 0, len(paths)+1)
	errs = append(errs, nonEmpty(field, paths))
	for _, p := range paths {
		errs = append(errs, validate.HealthPath(p))
	}
	return errors.Join(errs...)
}

func (d DAST) validate(app string) error {
	field := "apps." + app + ".dast."
	errs := []error{positive(field+"spider", int64(d.Spider)), positive(field+"max_time", int64(d.MaxTime))}
	for _, p := range []string{d.OpenAPI, d.Target} {
		if p != "" {
			errs = append(errs, validate.HealthPath(p))
		}
	}
	return errors.Join(errs...)
}

func validCPUs(field, v string) error {
	if !cpus.MatchString(v) {
		return fmt.Errorf("%w: %s must be a decimal CPU count", ErrPolicy, field)
	}
	return nil
}

func (s Scenario) validate(app string, seen map[string]bool) error {
	if s.ID == "" || seen[s.ID] {
		return fmt.Errorf("%w: app %s: scenario id %q is empty or repeated", ErrPolicy, app, s.ID)
	}
	seen[s.ID] = true
	if s.Category != CategorySecurity && s.Category != CategoryBusiness {
		return fmt.Errorf("%w: app %s: scenario %s: unknown category %q", ErrPolicy, app, s.ID, s.Category)
	}
	return nonEmpty("apps."+app+".scenarios."+s.ID+".tests", s.Tests)
}

func (a App) HasImage() bool {
	return a.Kind != KindBinary
}

func (a App) HasHTTP() bool {
	return a.Kind == KindService
}

func validData(app string, dirs []string) error {
	seen := map[string]bool{}
	for _, dir := range dirs {
		if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/" || strings.ContainsAny(dir, ",=") || seen[dir] {
			return fmt.Errorf("%w: app %s: data directory %q must be a unique clean absolute path other than / without commas or equal signs", ErrPolicy, app, dir)
		}
		seen[dir] = true
	}
	return nil
}
