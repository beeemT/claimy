// Package main starts the Claimy HTTP service.
package main

import (
	_ "time/tzdata"

	"github.com/beeemT/claimy/internal/api"
	"github.com/beeemT/claimy/internal/application"
	"github.com/gosoline-project/httpserver"
)

func main() {
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
