package cli

import (
	"context"
	"runtime/debug"

	"github.com/DreamsAreReal/deploy-hub/internal/mcp"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	usageMCP  = "usage: deploy-hub mcp   (no arguments; speaks MCP on stdin/stdout)"
	msgMCPEnv = "mcp: set VPS_ADDR to the VPS address (and DEPLOY_KEY to the deploy key if not ~/.ssh/deploy_hub_key)"
	devel     = "devel"
	sshOption = "-o"
)

func runMCP(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) != 0 {
		a.Err.Println(usageMCP)
		return ExitUsage
	}
	if a.Config.VPSAddr == "" {
		a.Err.Println(msgMCPEnv)
		return ExitUsage
	}
	s := mcp.Server{Remote: a.deployChannel(ex), Version: buildVersion(), Log: a.Log}
	if err := s.Serve(ctx, a.IO.Stdin, a.Out); err != nil {
		return a.failWith("mcp: ", err)
	}
	return ExitOK
}

func (a *App) deployChannel(ex procexec.Runner) mcp.Remote {
	target := a.Config.DeployUser + "@" + a.Config.VPSAddr
	return func(ctx context.Context, request string) (mcp.Reply, error) {
		res, err := ex.Run(ctx, procexec.Command{Name: binSSH, Args: []string{
			"-i", a.Config.DeployKey, sshOption, "IdentitiesOnly=yes", sshOption, "BatchMode=yes", target, request,
		}})
		return mcp.Reply{Stdout: res.Stdout, Stderr: res.Stderr}, err
	}
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return devel
}
