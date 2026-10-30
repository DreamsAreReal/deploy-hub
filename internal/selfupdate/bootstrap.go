package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	SSHBinary    = "ssh"
	tokenOwner   = "root"
	tokenModeRW  = "600"
	tokenModeRO  = "400"
	statToken    = "stat -c %U:%a " + DefaultPATFile
	readToken    = "cat " + DefaultPATFile
	installedNot = "test ! -e " + DefaultBinary
	stageBinary  = "install -d -m 755 /opt/deploy-hub/bin && install -o root -g root -m 755 /dev/stdin " + DefaultBinary + candidateSuffix
	candidateRun = DefaultBinary + candidateSuffix + " " + VerbVersion
	placeBinary  = "mv -f " + DefaultBinary + candidateSuffix + " " + DefaultBinary
	dropBinary   = "rm -f " + DefaultBinary + candidateSuffix
)

var (
	ErrInstalled = errors.New(DefaultBinary + " exists: the hub updates itself, bootstrap only installs the first binary")
	ErrHostToken = errors.New(DefaultPATFile + " on the hub must belong to root with mode 600 or 400")
)

type Bootstrap struct {
	Source Source
	Host   string
	Exec   procexec.Runner
}

func (b Bootstrap) Run(ctx context.Context, tag string) (Release, error) {
	if _, err := b.remote(ctx, "", installedNot); err != nil {
		return Release{}, ErrInstalled
	}
	token, err := b.token(ctx)
	if err != nil {
		return Release{}, err
	}
	b.Source.Token = token
	r, err := b.Source.Find(ctx, tag)
	var runs []int64
	if err == nil {
		runs, err = b.Source.Gated(ctx, r.Commit)
	}
	var bin []byte
	if err == nil {
		bin, err = b.Source.Fetch(ctx, r)
	}
	if err == nil {
		err = b.Source.Built(ctx, runs, bin)
	}
	if err != nil {
		return r, err
	}
	return r, b.install(ctx, r, bin)
}

func (b Bootstrap) token(ctx context.Context) (string, error) {
	st, err := b.remote(ctx, "", statToken)
	if err != nil {
		return "", fmt.Errorf("%w: %s is missing on the hub", ErrNoToken, DefaultPATFile)
	}
	if st != tokenOwner+":"+tokenModeRW && st != tokenOwner+":"+tokenModeRO {
		return "", fmt.Errorf("%w: got %s", ErrHostToken, st)
	}
	token, err := b.remote(ctx, "", readToken)
	if err != nil {
		return "", err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("%w: %s holds no single token", ErrNoToken, DefaultPATFile)
	}
	return token, nil
}

func (b Bootstrap) install(ctx context.Context, r Release, bin []byte) error {
	if _, err := b.remote(ctx, string(bin), stageBinary); err != nil {
		return err
	}
	got, err := b.remote(ctx, "", candidateRun)
	if err == nil && got != r.Commit {
		err = fmt.Errorf("%w: got %q want %s", ErrVersion, got, r.Commit)
	}
	if err == nil {
		_, err = b.remote(ctx, "", placeBinary)
	}
	if err != nil {
		_, _ = b.remote(context.WithoutCancel(ctx), "", dropBinary)
	}
	return err
}

func (b Bootstrap) remote(ctx context.Context, stdin, command string) (string, error) {
	res, err := b.Exec.Run(ctx, procexec.Command{Name: SSHBinary, Args: []string{b.Host, command}, Stdin: stdin})
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, procexec.Output(err))
	}
	return strings.TrimSpace(res.Stdout), nil
}
