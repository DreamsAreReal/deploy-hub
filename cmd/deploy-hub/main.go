package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/DreamsAreReal/deploy-hub/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Main(ctx, os.Args[1:], os.LookupEnv, cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	stop()
	os.Exit(code)
}
