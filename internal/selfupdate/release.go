package selfupdate

import (
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
	releaseTag   = regexp.MustCompile(`^sha-([0-9a-f]{40})$`)
	sumLine      = regexp.MustCompile(`^([0-9a-f]{64})(\s|$)`)
	assetID      = regexp.MustCompile(`^[0-9]{1,20}$`)
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
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Conclusion string `json:"conclusion"`
		Path       string `json:"path"`
	} `json:"workflow_runs"`
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

func (s Source) Find(ctx context.Context, tag string) (Release, error) {
	path := "/releases/latest"
	if tag != "" {
		path = "/releases/tags/" + url.PathEscape(tag)
	}
	data, err := s.get(ctx, s.API+"/repos/"+s.Repo+path, acceptJSON, maxDocument)
	if err != nil {
		return Release{}, err
	}
	var doc releaseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
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

func (s Source) Gated(ctx context.Context, commit string) error {
	q := url.Values{"head_sha": {commit}, "branch": {mainBranch}, "event": {pushEvent}, "status": {success}}
	data, err := s.get(ctx, s.API+"/repos/"+s.Repo+"/actions/workflows/"+Workflow+"/runs?"+q.Encode(), acceptJSON, maxDocument)
	if err != nil {
		return err
	}
	var doc runsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	for _, r := range doc.Runs {
		if r.HeadSHA == commit && r.HeadBranch == mainBranch && r.Event == pushEvent && r.Conclusion == success &&
			strings.HasSuffix(r.Path, "/"+Workflow) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotGated, commit)
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
	data, err := s.get(ctx, s.API+"/repos/"+s.Repo+"/compare/"+base+"..."+head, acceptJSON, maxDocument)
	if err != nil {
		return err
	}
	var doc struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
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
