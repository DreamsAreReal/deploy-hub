package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const rollbackCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rollback","arguments":{"app":"web","tag":"sha-abc1234"}}}`

func (h *harness) runMCP(stdin io.Reader, stdout io.Writer) int {
	deps := Deps{Exec: func(...string) procexec.Runner { return h.exec }, Owner: ChownTo, Now: time.Now}
	return Run(context.Background(), []string{CmdMCP}, h.lookup, IO{Stdin: stdin, Stdout: stdout, Stderr: h.err}, deps)
}

func TestMCPSendsToolCallsOverTheDeployChannel(t *testing.T) {
	h := newHarness(t)
	h.env[envDeployKey] = "/keys/deploy"
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: "rolled back (abc1234)\n"}, nil
	}
	testutil.Equal(t, h.runMCP(strings.NewReader(rollbackCall+"\n"), h.out), ExitOK)
	testutil.Equal(t, strings.Join(h.exec.Lines(), "|"),
		"ssh -i /keys/deploy -o IdentitiesOnly=yes -o BatchMode=yes deploy@"+testAddr+" rollback web sha-abc1234")
	testutil.Contains(t, h.out.String(), `"id":1`, `rolled back (abc1234)`, `"isError":false`)
}

func TestMCPArgumentsAndConfig(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdMCP, "extra"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageMCP)
	delete(h.env, envVPSAddr)
	testutil.Equal(t, h.run(CmdMCP), ExitUsage)
	testutil.Contains(t, h.err.String(), "mcp: set VPS_ADDR")
	testutil.Equal(t, len(h.exec.Calls()), 0)
}

func TestMCPFailsWhenStdoutBreaks(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.runMCP(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), brokenWriter{}), ExitFail)
	testutil.Contains(t, h.err.String(), "mcp: "+errSim.Error())
}

func TestBuildVersion(t *testing.T) {
	testutil.True(t, buildVersion() != "", "version is never empty")
}
