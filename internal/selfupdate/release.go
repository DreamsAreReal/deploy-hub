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
	AssetBinary     = "deploy-hub-linux-amd64"
	AssetSum        = AssetBinary + ".sha256"
	TagPrefix       = "sha-"
	Workflow        = "ci.yml"
	DefaultAPI      = "https://api.github.com"
	DefaultDownload = "https://github.com"
	maxBinary       = 128 << 20
	maxDocument     = 1 << 20
	httpTimeout     = 5 * time.Minute
	acceptJSON      = "application/vnd.github+json"
	success         = "success"
	mainBranch      = "main"
	pushEvent       = "push"
)

var (
	ErrTag       = errors.New("release tag must be sha-<40 hex> and target that commit")
	ErrAsset     = errors.New("release lacks its binary or checksum")
	ErrNotGated  = errors.New("no successful ci.yml run on main for the release commit")
	ErrChecksum  = errors.New("binary checksum does not match the release")
	ErrTooLarge  = errors.New("download is larger than allowed")
	ErrHTTP      = errors.New("unexpected HTTP status")
	ErrSumFormat = errors.New("checksum file must hold one sha256")
	releaseTag   = regexp.MustCompile(`^sha-([0-9a-f]{40})$`)
	sumLine      = regexp.MustCompile(`^([0-9a-f]{64})(\s|$)`)
)

type Source struct {
	API      string
	Download string
	Repo     string
	Client   *http.Client
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
		URL  string `json:"browser_download_url"`
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

func (s Source) get(ctx context.Context, raw string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptJSON)
	resp, err := s.Client.Do(req)
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
	data, err := s.get(ctx, s.API+"/repos/"+s.Repo+path, maxDocument)
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
	prefix := s.Download + "/" + s.Repo + "/releases/download/" + doc.TagName + "/"
	urls := map[string]string{}
	for _, a := range doc.Assets {
		if a.URL == prefix+a.Name {
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
	data, err := s.get(ctx, s.API+"/repos/"+s.Repo+"/actions/workflows/"+Workflow+"/runs?"+q.Encode(), maxDocument)
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
	sums, err := s.get(ctx, r.Sum, maxDocument)
	if err != nil {
		return nil, err
	}
	m := sumLine.FindStringSubmatch(strings.TrimSpace(string(sums)))
	if m == nil {
		return nil, ErrSumFormat
	}
	bin, err := s.get(ctx, r.Binary, maxBinary)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != m[1] {
		return nil, fmt.Errorf("%w: %s", ErrChecksum, r.Tag)
	}
	return bin, nil
}
