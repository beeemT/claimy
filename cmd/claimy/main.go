// Package main starts the Claimy HTTP service.
package main

import (
	"context"
	"os"
	_ "time/tzdata"

	"github.com/beeemT/claimy/internal/api"
	"github.com/beeemT/claimy/internal/application"
	"github.com/beeemT/claimy/internal/cli"
	"github.com/gosoline-project/httpserver"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "api" {
		os.Exit(cli.Run(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
	}

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
