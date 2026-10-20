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

	percentCeiling = 100
	purlScheme     = "pkg:"
	policyChecks   = 7
	appChecks      = 3
)

var (
	cpus      = regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,2})?$`)
	digestRef = regexp.MustCompile(`^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$`)
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
}

type Images struct {
	Semgrep string `yaml:"semgrep"`
	ZAP     string `yaml:"zap"`
}

type Thresholds struct {
	LineCoverage    float64 `yaml:"line_coverage"`
	BranchCoverage  float64 `yaml:"branch_coverage"`
	MutationScore   float64 `yaml:"mutation_score"`
	MaxCognitive    int     `yaml:"max_cognitive_complexity"`
	MaxDuplication  float64 `yaml:"max_duplication_percent"`
	DuplicateTokens int     `yaml:"duplicate_min_tokens"`
}

type Limits struct {
	Check          time.Duration `yaml:"check_timeout"`
	LongCheck      time.Duration `yaml:"long_check_timeout"`
	FuzzTime       time.Duration `yaml:"fuzz_time"`
	FuzzExamples   int           `yaml:"fuzz_examples"`
	MutantFloor    time.Duration `yaml:"mutant_timeout_floor"`
	MutantFactor   int           `yaml:"mutant_timeout_factor"`
	FindingsListed int           `yaml:"findings_listed"`
	StageReady     time.Duration `yaml:"stage_ready_timeout"`
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
	Scenarios []Scenario  `yaml:"scenarios"`
	Load      LoadProfile `yaml:"load"`
	Localized []string    `yaml:"localized"`
	Allowed   []string    `yaml:"allowed_files"`
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
		p.validateDeclared(), p.validateReviewedTexts(),
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
	)
}

func (i Images) validate() error {
	return errors.Join(pinned("images.semgrep", i.Semgrep), pinned("images.zap", i.ZAP))
}

func pinned(field, ref string) error {
	if !digestRef.MatchString(ref) {
		return fmt.Errorf("%w: %s must be pinned by sha256 digest", ErrPolicy, field)
	}
	return nil
}

func (a App) validate(name string) error {
	errs := make([]error, 0, len(a.Scenarios)+appChecks)
	errs = append(errs, validate.AppName(name), validate.RepoSlug(a.Repo), a.validateKind(name))
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
	return errors.Join(errs...)
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
			validCPUs("apps."+name+".cpus", a.CPUs), a.Load.validate(name), a.DAST.validate(name),
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
