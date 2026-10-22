package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
	"github.com/DreamsAreReal/deploy-hub/internal/web"
)

const (
	DefaultWebPort    = "8970"
	webReadHeader     = 10 * time.Second
	webShutdown       = 10 * time.Second
	webPrefix         = "web: "
	msgWebServing     = "web: dashboard on http://%s\n"
	usageWebPortValue = "loopback port to serve the dashboard on"
)

func runWeb(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	fs := flag.NewFlagSet(CmdWeb, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	port := fs.String("port", DefaultWebPort, usageWebPortValue)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if err := validate.Port(*port); err != nil {
		return a.failWith(webPrefix, err)
	}
	srv := &web.Server{
		Snapshot: a.runnerService(ex, a.Out, a.Err).Snapshot,
		Journal:  a.journal().Entries,
		Act:      a.act(ex),
		Now:      a.Now,
		Log:      a.Log,
		HostSlug: a.Config.Caddy.HostSlug,
	}
	return a.serve(ctx, srv, net.JoinHostPort(netprobe.Loopback, *port))
}

func (a *App) act(ex procexec.Runner) web.Actor {
	return func(ctx context.Context, request string, out io.Writer) int {
		p := console.New(out)
		return a.runnerService(ex, p, p).Dispatch(ctx, request, strings.NewReader(""))
	}
}

func (a *App) serve(ctx context.Context, srv *web.Server, addr string) int {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return a.failWith(webPrefix, err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: webReadHeader}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webShutdown)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	a.Out.Printf(msgWebServing, ln.Addr())
	a.Log.Info("web listening", "addr", ln.Addr().String())
	err = hs.Serve(ln)
	srv.Wait()
	if !errors.Is(err, http.ErrServerClosed) {
		return a.failWith(webPrefix, err)
	}
	a.Log.Info("web stopped")
	return ExitOK
}
