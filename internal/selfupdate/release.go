package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	AssetBinary  = "deploy-hub-linux-amd64"
	AssetSum     = AssetBinary + ".sha256"
	BuiltEntry   = "deploy-hub"
	TagPrefix    = "sha-"
	Workflow     = "ci.yml"
	DefaultAPI   = "https://api.github.com"
	maxBinary    = 128 << 20
	maxDocument  = 1 << 20
	httpTimeout  = 5 * time.Minute
	acceptJSON   = "application/vnd.github+json"
	acceptBinary = "application/octet-stream"
	apiVersion   = "2022-11-28"
	success      = "success"
	mainBranch   = "main"
	pushEvent    = "push"
	compareAhead = "ahead"
	compareSame  = "identical"
	maxRedirects = 10
)

var (
	ErrTag       = errors.New("release tag must be sha-<40 hex> and target that commit")
	ErrAsset     = errors.New("release lacks its binary or checksum")
	ErrNotGated  = errors.New("no successful ci.yml run on main for the release commit")
	ErrChecksum  = errors.New("binary checksum does not match the release")
	ErrTooLarge  = errors.New("download is larger than allowed")
	ErrHTTP      = errors.New("unexpected HTTP status")
	ErrSumFormat = errors.New("checksum file must hold one sha256")
	ErrNoToken   = errors.New("no GitHub token: the private repo needs one to read releases and CI runs")
	ErrRewritten = errors.New("release commit does not descend from the installed one: main was rewritten")
	ErrRedirects = errors.New("too many redirects")
	ErrNotBuilt  = errors.New("the release binary is not the one the gated CI run built")
	releaseTag   = regexp.MustCompile(`^sha-([0-9a-f]{40})$`)
	sumLine      = regexp.MustCompile(`^([0-9a-f]{64})(\s|$)`)
	assetID      = regexp.MustCompile(`^[0-9]{1,20}$`)
	artifactZip  = regexp.MustCompile(`^[0-9]{1,20}/zip$`)
	fullCommit   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Source struct {
	API    string
	Repo   string
	Token  string
	Client *http.Client
}

type Release struct {
	Tag    string
	Commit string
	Binary string
	Sum    string
}

type releaseDoc struct {
	TagName string `json:"tag_name"`
	Target  string `json:"target_commitish"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

type runsDoc struct {
	Runs []struct {
		ID         int64  `json:"id"`
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Conclusion string `json:"conclusion"`
		Path       string `json:"path"`
	} `json:"workflow_runs"`
}

type artifactsDoc struct {
	Artifacts []struct {
		Name    string `json:"name"`
		Expired bool   `json:"expired"`
		URL     string `json:"archive_download_url"`
	} `json:"artifacts"`
}

func ValidTag(tag string) bool {
	return releaseTag.MatchString(tag)
}

func (s Source) get(ctx context.Context, raw, accept string, limit int64) ([]byte, error) {
	if s.Token == "" {
		return nil, ErrNoToken
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	client := *s.Client
	client.CheckRedirect = dropTokenOffHost
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: %s", ErrHTTP, raw, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%w: %s", ErrTooLarge, raw)
	}
	return data, err
}

func (s Source) getJSON(ctx context.Context, raw string, out any) error {
	data, err := s.get(ctx, raw, acceptJSON, maxDocument)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (s Source) Find(ctx context.Context, tag string) (Release, error) {
	path := "/releases/latest"
	if tag != "" {
		path = "/releases/tags/" + url.PathEscape(tag)
	}
	var doc releaseDoc
	if err := s.getJSON(ctx, s.API+"/repos/"+s.Repo+path, &doc); err != nil {
		return Release{}, err
	}
	return s.release(doc, tag)
}

func (s Source) release(doc releaseDoc, want string) (Release, error) {
	m := releaseTag.FindStringSubmatch(doc.TagName)
	if m == nil || doc.Target != m[1] || (want != "" && want != doc.TagName) {
		return Release{}, fmt.Errorf("%w: %q -> %q", ErrTag, doc.TagName, doc.Target)
	}
	r := Release{Tag: doc.TagName, Commit: m[1]}
	prefix := s.API + "/repos/" + s.Repo + "/releases/assets/"
	urls := map[string]string{}
	for _, a := range doc.Assets {
		if id, ok := strings.CutPrefix(a.URL, prefix); ok && assetID.MatchString(id) {
			urls[a.Name] = a.URL
		}
	}
	r.Binary, r.Sum = urls[AssetBinary], urls[AssetSum]
	if r.Binary == "" || r.Sum == "" {
		return Release{}, fmt.Errorf("%w: %s", ErrAsset, doc.TagName)
	}
	return r, nil
}

func (s Source) Gated(ctx context.Context, commit string) ([]int64, error) {
	q := url.Values{"head_sha": {commit}, "branch": {mainBranch}, "event": {pushEvent}, "status": {success}}
	var doc runsDoc
	if err := s.getJSON(ctx, s.API+"/repos/"+s.Repo+"/actions/workflows/"+Workflow+"/runs?"+q.Encode(), &doc); err != nil {
		return nil, err
	}
	var runs []int64
	for _, r := range doc.Runs {
		if r.HeadSHA == commit && r.HeadBranch == mainBranch && r.Event == pushEvent && r.Conclusion == success &&
			strings.HasSuffix(r.Path, "/"+Workflow) {
			runs = append(runs, r.ID)
		}
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotGated, commit)
	}
	return runs, nil
}

func (s Source) Built(ctx context.Context, runs []int64, bin []byte) error {
	for _, run := range runs {
		built, err := s.runBuilt(ctx, run, bin)
		if err != nil || built {
			return err
		}
	}
	return fmt.Errorf("%w: no artifact %s of runs %v holds it", ErrNotBuilt, AssetBinary, runs)
}

func (s Source) runBuilt(ctx context.Context, run int64, bin []byte) (bool, error) {
	var doc artifactsDoc
	query := url.Values{"name": {AssetBinary}}.Encode()
	if err := s.getJSON(ctx, fmt.Sprintf("%s/repos/%s/actions/runs/%d/artifacts?%s", s.API, s.Repo, run, query), &doc); err != nil {
		return false, err
	}
	prefix := s.API + "/repos/" + s.Repo + "/actions/artifacts/"
	for _, a := range doc.Artifacts {
		if id, ok := strings.CutPrefix(a.URL, prefix); !ok || a.Name != AssetBinary || a.Expired || !artifactZip.MatchString(id) {
			continue
		}
		built, err := s.builtBinary(ctx, a.URL)
		if err != nil {
			return false, err
		}
		if bytes.Equal(built, bin) {
			return true, nil
		}
	}
	return false, nil
}

func (s Source) builtBinary(ctx context.Context, raw string) ([]byte, error) {
	archive, err := s.get(ctx, raw, acceptBinary, maxBinary)
	if err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotBuilt, err)
	}
	for _, file := range reader.File {
		if file.Name == BuiltEntry && file.UncompressedSize64 <= maxBinary {
			return readEntry(file)
		}
	}
	return nil, fmt.Errorf("%w: the artifact has no %s", ErrNotBuilt, BuiltEntry)
}

func readEntry(file *zip.File) ([]byte, error) {
	body, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return io.ReadAll(io.LimitReader(body, maxBinary))
}

func (s Source) Fetch(ctx context.Context, r Release) ([]byte, error) {
	sums, err := s.get(ctx, r.Sum, acceptBinary, maxDocument)
	if err != nil {
		return nil, err
	}
	m := sumLine.FindStringSubmatch(strings.TrimSpace(string(sums)))
	if m == nil {
		return nil, ErrSumFormat
	}
	bin, err := s.get(ctx, r.Binary, acceptBinary, maxBinary)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != m[1] {
		return nil, fmt.Errorf("%w: %s", ErrChecksum, r.Tag)
	}
	return bin, nil
}

func (s Source) Descends(ctx context.Context, base, head string) error {
	if !fullCommit.MatchString(base) || base == head {
		return nil
	}
	var doc struct {
		Status string `json:"status"`
	}
	if err := s.getJSON(ctx, s.API+"/repos/"+s.Repo+"/compare/"+base+"..."+head, &doc); err != nil {
		return err
	}
	if doc.Status != compareAhead && doc.Status != compareSame {
		return fmt.Errorf("%w: %s -> %s is %q", ErrRewritten, base, head, doc.Status)
	}
	return nil
}

func dropTokenOffHost(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return ErrRedirects
	}
	if req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}
