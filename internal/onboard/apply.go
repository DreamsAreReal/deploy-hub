package onboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/provision"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	stubDirPerm   = 0o750
	stubFilePerm  = 0o600
	remoteVerb    = "provision"
	gitConfigFlag = "-c"
)

func (s *session) request() provision.Request {
	return provision.Request{
		App: s.opts.App, Profile: s.opts.Profile, Port: s.opts.Port, CPort: s.opts.CPort,
		Mem: s.opts.Mem, HealthPath: s.opts.HealthPath, Image: s.image, Repo: s.opts.Repo,
	}
}

func (s *session) plan() []string {
	var out []string
	add := func(cond bool, format string, args ...any) {
		if cond {
			out = append(out, fmt.Sprintf(format, args...))
		}
	}
	f, dir := s.server, s.server.Dir
	add(s.stubState != stubStateOK, planStub, s.opts.Repo, s.branch, s.stubState)
	for _, name := range []string{deployKeyName, repoVarHost, repoVarHostKey} {
		add(!s.secrets[name], planRepoKey, name, s.opts.Repo)
	}
	add(!f.DirExists, planDir, dir)
	container := s.opts.Profile != registry.ProfileHost
	add(container && !f.ComposeExists, planCompose, dir, s.image, s.portsNote(), s.opts.Mem)
	add(!f.ConfExists, planConf, dir, s.opts.Profile, s.confPortNote(), s.opts.HealthPath)
	add(container && !f.EnvExists, planEnv, dir)
	add(!f.InList, planList, s.opts.App, dir)
	return out
}

func (s *session) portsNote() string {
	if s.opts.Port == "" {
		return ""
	}
	return fmt.Sprintf(planComposePorts, s.opts.Port, s.opts.CPort)
}

func (s *session) confPortNote() string {
	if s.opts.Port == "" {
		return ""
	}
	return fmt.Sprintf(planConfPort, s.opts.Port)
}

func (o Onboarder) apply(ctx context.Context, s *session) error {
	o.Out.Println(msgApplying)
	if err := o.setSecrets(ctx, s); err != nil {
		return err
	}
	out, err := o.remote(ctx, append([]string{remoteVerb}, s.request().Args()...)...)
	if err != nil {
		return err
	}
	o.Out.Print(out)
	if s.stubState == stubStateOK {
		return nil
	}
	return o.commitStub(ctx, s)
}

type secretSource struct {
	name  string
	value func(ctx context.Context) (string, error)
}

func (o Onboarder) secretSources() []secretSource {
	return []secretSource{
		{name: deployKeyName, value: o.deployKey},
		{name: repoVarHost, value: func(context.Context) (string, error) { return o.Config.Addr, nil }},
		{name: repoVarHostKey, value: o.hostKey},
	}
}

func (o Onboarder) setSecrets(ctx context.Context, s *session) error {
	for _, src := range o.secretSources() {
		if s.secrets[src.name] {
			continue
		}
		value, err := src.value(ctx)
		if err != nil {
			return err
		}
		if err := o.storeSecret(ctx, s, src.name, value); err != nil {
			return err
		}
	}
	return nil
}

func (o Onboarder) storeSecret(ctx context.Context, s *session, name, value string) error {
	_, err := o.Exec.Run(ctx, procexec.Command{Name: ghBinary, Args: []string{"secret", "set", name, "-R", s.opts.Repo}, Stdin: value})
	if err != nil {
		return err
	}
	o.Out.Printf(msgKeyStored, name)
	o.Log.Info("repo secret set", "repo", s.opts.Repo, "secret", name)
	return nil
}

func (o Onboarder) deployKey(context.Context) (string, error) {
	key, err := os.ReadFile(o.Config.DeployKey)
	if err != nil {
		return "", fmt.Errorf(errNoKey, o.Config.DeployKey)
	}
	return string(key), nil
}

func (o Onboarder) hostKey(ctx context.Context) (string, error) {
	out, _ := o.run(ctx, SSHKeygen, "-F", o.Config.Addr)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, knownHostsComment) {
			return line, nil
		}
	}
	return "", fmt.Errorf(errNoHostKey, o.Config.Addr)
}

func (o Onboarder) git(ctx context.Context, s *session, args ...string) error {
	_, err := o.run(ctx, gitBinary, append([]string{"-C", s.repoPath()}, args...)...)
	return err
}

func (o Onboarder) commitStub(ctx context.Context, s *session) error {
	path := s.repoPath(s.stubPath)
	if err := os.MkdirAll(filepath.Dir(path), stubDirPerm); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(s.stubWant+"\n"), stubFilePerm); err != nil {
		return err
	}
	if err := o.git(ctx, s, "add", s.stubPath); err != nil {
		return err
	}
	err := o.git(ctx, s, gitConfigFlag, "user.name="+stubAuthorName, gitConfigFlag, "user.email="+stubAuthorEmail,
		"commit", "-q", "-m", stubCommitMessage)
	if err != nil {
		return err
	}
	if o.git(ctx, s, "push", "-q", "origin", s.branch) == nil {
		o.Out.Printf(msgStubCommitted, s.branch)
		return nil
	}
	return o.openPR(ctx, s)
}

func (o Onboarder) openPR(ctx context.Context, s *session) error {
	if err := o.git(ctx, s, "checkout", "-q", "-b", stubBranch); err != nil {
		return err
	}
	if err := o.git(ctx, s, "push", "-q", "-f", "origin", stubBranch); err != nil {
		return err
	}
	_, err := o.run(ctx, ghBinary, "pr", "create", "-R", s.opts.Repo, "--head", stubBranch, "--base", s.branch,
		"--title", stubCommitMessage, "--body", stubPRBody)
	if err == nil {
		o.Out.Println(msgProtected)
	}
	return err
}

func (o Onboarder) finish(s *session) {
	o.Out.Println(msgDone)
	o.Log.Info("onboard applied", "repo", s.opts.Repo, "app", s.opts.App)
	if s.opts.Profile == registry.ProfileStatic && len(s.envVars) == 0 {
		o.Out.Println(msgLeftNone)
		return
	}
	o.Out.Printf(msgLeftSecrets, s.server.Dir)
	if len(s.envVars) == 0 {
		o.Out.Println(msgVarsNone)
		return
	}
	o.Out.Printf(msgVarsFound, strings.Join(s.envVars, " "))
}
