// Command strata is the Strata command-line client.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/aws-abaschen/strata/backend/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dir, _ := os.Getwd()
	os.Exit(cli.Run(ctx, os.Args[1:], cli.Env{Dir: dir, Out: os.Stdout}))
}
