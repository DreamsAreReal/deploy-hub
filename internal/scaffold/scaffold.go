package scaffold

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/onboard"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	Usage = `usage: deploy-hub scaffold <app> --port N [--repo OWNER/NAME] [--dir DIR] [--template DIR]
       (run from a clone of deploy-hub: the template is read from templates/service;
        --port is the 127.0.0.1 port of the VPS that the app's compose publishes)`
	DefaultTemplate = "templates/service"
	placeholder     = "starter"
	placeholderRepo = "DreamsAreReal/" + placeholder
	placeholderPort = "127.0.0.1:9099:"
	loopbackPort    = "127.0.0.1:%s:"
	goModFile       = "go.mod"
	dockerfile      = "Dockerfile"
	deployBranch    = "main"
	slugSeparator   = "/"
	dirPerm         = 0o750
	filePerm        = 0o600
)

var (
	ErrUsage      = errors.New(Usage)
	ErrDirInUse   = errors.New("the target directory exists and is not empty")
	ErrNoTemplate = errors.New("no service template here (run from a clone of deploy-hub or pass --template)")
)

type Options struct {
	App      string
	Repo     string
	Port     string
	Dir      string
	Template string
}

func ParseOptions(args []string, owner string) (Options, error) {
	o := Options{}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return o, ErrUsage
	}
	o.App = args[0]
	flags := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.Port, "port", "", "")
	flags.StringVar(&o.Repo, "repo", owner+slugSeparator+o.App, "")
	flags.StringVar(&o.Dir, "dir", o.App, "")
	flags.StringVar(&o.Template, "template", DefaultTemplate, "")
	if flags.Parse(args[1:]) != nil || flags.NArg() > 0 || o.Port == "" {
		return o, ErrUsage
	}
	return o, errors.Join(validate.AppName(o.App), validate.RepoSlug(o.Repo), validate.Port(o.Port))
}

func (o Options) Owner() string {
	owner, _, _ := strings.Cut(o.Repo, slugSeparator)
	return owner
}

func (o Options) filler() *strings.Replacer {
	return strings.NewReplacer(placeholderRepo, o.Repo, placeholderPort, fmt.Sprintf(loopbackPort, o.Port), placeholder, o.App)
}

func Render(template fs.FS, o Options, hubRepo string) (map[string][]byte, error) {
	files := map[string][]byte{}
	fill := o.filler()
	err := fs.WalkDir(template, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(template, name)
		files[strings.ReplaceAll(name, placeholder, o.App)] = []byte(fill.Replace(string(body)))
		return err
	})
	if errors.Is(err, fs.ErrNotExist) || err == nil && files[goModFile] == nil {
		return nil, ErrNoTemplate
	}
	if err != nil {
		return nil, err
	}
	stub := onboard.Options{App: o.App, Dockerfile: dockerfile}
	files[onboard.StubPath(stub)] = []byte(onboard.Stub(deployBranch, hubRepo, stub))
	return files, nil
}

func Write(dir string, files map[string][]byte) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrDirInUse, dir)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
			return err
		}
		if err := os.WriteFile(target, files[name], filePerm); err != nil {
			return err
		}
	}
	return nil
}
