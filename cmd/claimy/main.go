// Package main starts the Claimy HTTP service.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/beeemT/claimy/internal/api"
	"github.com/beeemT/claimy/internal/application"
	"github.com/beeemT/claimy/internal/cli"
	"github.com/gosoline-project/httpserver"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		runServer()

		return
	}
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		os.Exit(runMigration())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	status := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}

func runMigration() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := application.NewMigrationConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claimy migration: %v\n", err)

		return 1
	}

	return application.RunMigration(ctx, config)
}

func runServer() {
	httpserver.RunServers(map[string]httpserver.ServerDefinition{
		"default": {
			RouterFactory: application.Register,
			Options: []httpserver.ServerOption{
				httpserver.WithErrorMapper(api.ErrorMapper),
				httpserver.WithErrorHandler(api.ErrorHandler),
			},
		},
	}, application.Options()...)
}
