// Package main starts the Claimy HTTP service.
package main

import (
	"context"
	_ "time/tzdata"

	"github.com/gosoline-project/httpserver"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
)

func main() {
	httpserver.RunDefaultServer(registerRoutes)
}

// The framework registers /health; claim routes are not declared here.
func registerRoutes(_ context.Context, _ cfg.Config, _ log.Logger, _ *httpserver.Router) error {
	return nil
}
