package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	SyftBinary        = "syft"
	SBOMSource        = "sbom-source.cdx.json"
	SBOMImage         = "sbom-image.cdx.json"
	ValueComponents   = "components"
	reasonLicenses    = "dependencies with unknown or unapproved licenses"
	reasonEmptySBOM   = "the SBOM lists no components"
	noLicenseFormat   = "%s: license unknown"
	badLicenseFormat  = "%s: license %s is not approved"
	orSeparator       = " OR "
	andSeparator      = " AND "
	cycloneOutput     = "cyclonedx-json="
	dirScheme         = "dir:"
	archiveScheme     = "docker-archive:"
	golangPurl        = "pkg:golang/"
	apache2           = "Apache-2.0"
	mit               = "MIT"
	bsd3              = "BSD-3-Clause"
	bsd2              = "BSD-2-Clause"
	mpl2              = "MPL-2.0"
	pypiPurl          = "pkg:pypi/"
	npmPurl           = "pkg:npm/"
	pyprojectManifest = "pyproject.toml"
	npmManifest       = "package.json"
	trackedCopy       = "sbom-src"
	nugetPurl         = "pkg:nuget/"
	nuspecExt         = ".nuspec"
	nugetPackagesEnv  = "NUGET_PACKAGES"
)

var (
	licensedEcosystems = []string{golangPurl, npmPurl, pypiPurl, nugetPurl, "pkg:maven/", "pkg:cargo/", "pkg:gem/"}
	syftLicenseEnv     = []string{
		"SYFT_GOLANG_SEARCH_LOCAL_MOD_CACHE_LICENSES=true", "SYFT_GOLANG_SEARCH_REMOTE_LICENSES=true",
		"SYFT_PYTHON_SEARCH_REMOTE_LICENSES=true", "SYFT_JAVASCRIPT_SEARCH_REMOTE_LICENSES=true", "SYFT_JAVASCRIPT_INCLUDE_DEV_DEPENDENCIES=true",
	}
	pyprojectName  = regexp.MustCompile(`(?ms)^\[project\][^\[]*?^name\s*=\s*["']([^"']+)["']`)
	pypiSeparator  = regexp.MustCompile(`[-_.]+`)
	nuspecLicense  = regexp.MustCompile(`<license type="expression">([^<]+)</license>|<licenseUrl>https://licenses\.nuget\.org/([^<]+)</licenseUrl>`)
	dotnetProjects = []string{".csproj", ".fsproj", ".vbproj"}
)

type cycloneDX struct {
	Components []component `json:"components"`
}

type component struct {
	Name     string `json:"name"`
	Purl     string `json:"purl"`
	Licenses []struct {
		License struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"license"`
		Expression string `json:"expression"`
	} `json:"licenses"`
}

func SBOM(ctx context.Context, ws gate.Workspace) gate.Outcome {
	tracked := ws.Report(trackedCopy)
	defer func() { _ = os.RemoveAll(tracked) }()
	if err := source.CopyTracked(ctx, ws, tracked); err != nil {
		return gate.Fail(err.Error())
	}
	targets := map[string]string{SBOMSource: dirScheme + tracked}
	if ws.Image != "" {
		archive, err := imageArchive(ctx, ws)
		if err != nil {
			return gate.Fail(err.Error())
		}
		targets[SBOMImage] = archiveScheme + archive
	}
	var comps []component
	for _, name := range sortedKeys(targets) {
		got, err := sbomOf(ctx, ws, targets[name], ws.Report(name))
		if err != nil {
			return gate.Fail(err.Error())
		}
		comps = append(comps, got...)
	}
	if len(comps) == 0 {
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonEmptySBOM}
	}
	selves, err := selfPurls(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	findings := licenseFindings(comps, ws.Policy, selves)
	values := map[string]float64{ValueComponents: float64(len(comps)), gate.ValueFindings: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonLicenses, values, findings)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sbomOf(ctx context.Context, ws gate.Workspace, target, report string) ([]component, error) {
	_, err := ws.RunEnv(ctx, syftLicenseEnv, SyftBinary, "scan", target, "-o", cycloneOutput+report, "-q")
	if err != nil {
		return nil, fmt.Errorf("syft %s: %w: %s", target, err, gate.Tail(procexec.Output(err)))
	}
	data, err := os.ReadFile(filepath.Clean(report))
	if err != nil {
		return nil, err
	}
	var bom cycloneDX
	if err := json.Unmarshal(data, &bom); err != nil {
		return nil, fmt.Errorf("SBOM unreadable: %w", err)
	}
	return bom.Components, nil
}

func licenseFindings(comps []component, pol policy.Policy, selves []string) []string {
	ok := map[string]bool{}
	for _, l := range pol.Licenses {
		ok[l] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range comps {
		if !licensed(c.Purl) || seen[c.Purl] || isSelf(c.Purl, selves) {
			continue
		}
		seen[c.Purl] = true
		out = append(out, judgeLicenses(c, ok, pol.Declared)...)
	}
	return out
}

func isSelf(purl string, selves []string) bool {
	if strings.HasPrefix(purl, nugetPurl) {
		purl = strings.ToLower(purl)
	}
	for _, own := range selves {
		if purl == own || strings.HasPrefix(purl, own+"@") {
			return true
		}
	}
	return false
}

func selfPurls(ctx context.Context, ws gate.Workspace) ([]string, error) {
	var out []string
	if ws.Module != "" {
		out = append(out, golangPurl+ws.Module)
	}
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	for _, f := range files {
		data, rerr := os.ReadFile(filepath.Join(ws.Src, filepath.FromSlash(f)))
		if rerr != nil {
			return nil, rerr
		}
		out = append(out, manifestPurls(path.Base(f), data)...)
	}
	return out, err
}

func manifestPurls(base string, data []byte) []string {
	if ext := path.Ext(base); slices.Contains(dotnetProjects, ext) {
		return []string{nugetPurl + strings.ToLower(strings.TrimSuffix(base, ext))}
	}
	switch base {
	case pyprojectManifest:
		if m := pyprojectName.FindSubmatch(data); m != nil {
			return []string{pypiPurl + pypiSeparator.ReplaceAllString(strings.ToLower(string(m[1])), "-")}
		}
	case npmManifest:
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			return []string{npmPurl + strings.Replace(pkg.Name, "@", "%40", 1)}
		}
	}
	return nil
}

func licensed(purl string) bool {
	for _, p := range licensedEcosystems {
		if strings.HasPrefix(purl, p) {
			return true
		}
	}
	return false
}

func judgeLicenses(c component, ok map[string]bool, declared map[string]string) []string {
	exprs := make([]string, 0, len(c.Licenses))
	for _, l := range c.Licenses {
		exprs = append(exprs, canonical(firstNonEmpty(l.License.ID, l.License.Name, l.Expression)))
	}
	if d, found := declared[purlName(c.Purl)]; found && len(exprs) == 0 {
		exprs = append(exprs, d)
	}
	if nuspec := nugetLicense(c.Purl); nuspec != "" && len(exprs) == 0 {
		exprs = append(exprs, nuspec)
	}
	if len(exprs) == 0 {
		return []string{fmt.Sprintf(noLicenseFormat, c.Purl)}
	}
	var out []string
	for _, expr := range exprs {
		if !expressionAllowed(expr, ok) {
			out = append(out, fmt.Sprintf(badLicenseFormat, c.Purl, expr))
		}
	}
	return out
}

func nugetLicense(purl string) string {
	name, ok := strings.CutPrefix(purlName(purl), nugetPurl)
	_, version, _ := strings.Cut(purl, "@")
	if !ok || version == "" {
		return ""
	}
	id := strings.ToLower(name)
	data, err := os.ReadFile(filepath.Clean(filepath.Join(nugetRoot(), id, strings.ToLower(version), id+nuspecExt)))
	if err != nil {
		return ""
	}
	if m := nuspecLicense.FindSubmatch(data); m != nil {
		return strings.TrimSpace(string(m[1]) + string(m[2]))
	}
	return ""
}

func nugetRoot() string {
	if root := os.Getenv(nugetPackagesEnv); root != "" {
		return root
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nuget", "packages")
}

func purlName(purl string) string {
	name, _, _ := strings.Cut(purl, "@")
	return name
}

var licenseAliases = map[string]string{
	"apache 2.0": apache2, "apache 2": apache2, "apache-2": apache2, "apache license 2.0": apache2, "apache license, version 2.0": apache2,
	"apache license version 2.0": apache2, "apache software license": apache2, "apache software license 2.0": apache2,
	"mit license": mit, "the mit license": mit, "mit/x11": mit, "expat": mit,
	"bsd 3-clause": bsd3, "bsd-3": bsd3, "new bsd": bsd3, "new bsd license": bsd3, "bsd 3-clause license": bsd3, "3-clause bsd": bsd3,
	"modified bsd license": bsd3, "bsd 2-clause": bsd2, "bsd-2": bsd2, "simplified bsd": bsd2, "bsd 2-clause license": bsd2,
	"isc license": "ISC", "mozilla public license 2.0": mpl2, "mpl 2.0": mpl2, "the unlicense": "Unlicense", "zlib license": "Zlib",
}

func canonical(expr string) string {
	if spdx, ok := licenseAliases[strings.ToLower(strings.TrimSpace(expr))]; ok {
		return spdx
	}
	return expr
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func expressionAllowed(expr string, ok map[string]bool) bool {
	for _, alt := range strings.Split(strings.Trim(expr, "()"), orSeparator) {
		if allAllowed(strings.Split(alt, andSeparator), ok) {
			return true
		}
	}
	return false
}

func allAllowed(ids []string, ok map[string]bool) bool {
	for _, id := range ids {
		if !ok[strings.Trim(strings.TrimSpace(id), "()")] {
			return false
		}
	}
	return true
}
