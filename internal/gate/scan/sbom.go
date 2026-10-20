package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	SyftBinary       = "syft"
	SBOMSource       = "sbom-source.cdx.json"
	SBOMImage        = "sbom-image.cdx.json"
	ValueComponents  = "components"
	reasonLicenses   = "dependencies with unknown or unapproved licenses"
	reasonEmptySBOM  = "the SBOM lists no components"
	noLicenseFormat  = "%s: license unknown"
	badLicenseFormat = "%s: license %s is not approved"
	orSeparator      = " OR "
	andSeparator     = " AND "
	cycloneOutput    = "cyclonedx-json="
	dirScheme        = "dir:"
	archiveScheme    = "docker-archive:"
	golangPurl       = "pkg:golang/"
)

var (
	licensedEcosystems = []string{golangPurl, "pkg:npm/", "pkg:pypi/", "pkg:nuget/", "pkg:maven/", "pkg:cargo/", "pkg:gem/"}
	syftLicenseEnv     = []string{"SYFT_GOLANG_SEARCH_LOCAL_MOD_CACHE_LICENSES=true", "SYFT_GOLANG_SEARCH_REMOTE_LICENSES=true"}
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
	targets := map[string]string{SBOMSource: dirScheme + ws.Src}
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
		return gate.Fail(reasonEmptySBOM)
	}
	findings := licenseFindings(comps, ws.Policy.Licenses, ws.Module)
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

func licenseFindings(comps []component, allowed []string, self string) []string {
	ok := map[string]bool{}
	for _, l := range allowed {
		ok[l] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range comps {
		if !licensed(c.Purl) || seen[c.Purl] || isSelf(c.Purl, self) {
			continue
		}
		seen[c.Purl] = true
		out = append(out, judgeLicenses(c, ok)...)
	}
	return out
}

func isSelf(purl, self string) bool {
	own := golangPurl + self
	return self != "" && (purl == own || strings.HasPrefix(purl, own+"@"))
}

func licensed(purl string) bool {
	for _, p := range licensedEcosystems {
		if strings.HasPrefix(purl, p) {
			return true
		}
	}
	return false
}

func judgeLicenses(c component, ok map[string]bool) []string {
	if len(c.Licenses) == 0 {
		return []string{fmt.Sprintf(noLicenseFormat, c.Purl)}
	}
	var out []string
	for _, l := range c.Licenses {
		expr := firstNonEmpty(l.License.ID, l.License.Name, l.Expression)
		if !expressionAllowed(expr, ok) {
			out = append(out, fmt.Sprintf(badLicenseFormat, c.Purl, expr))
		}
	}
	return out
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
