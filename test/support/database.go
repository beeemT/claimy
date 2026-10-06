//go:build integration && fixtures

// Package support provides shared integration-test fixtures.
package support

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/appctx"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
	"github.com/justtrackio/gosoline/pkg/test/env"
	"github.com/pressly/goose/v3"
)

const mysqlImageName = "mysql"

// Fixture owns one disposable MySQL instance and the SQLC client used by the
// application. Each call starts an independent mysql:8.0.42 container.
type Fixture struct {
	Context context.Context
	Config  cfg.Config
	Logger  log.Logger
	Client  sqlc.Client
	SQLDB   *sql.DB
}

// NewFixture starts MySQL, applies Claimy migrations and owns fixture cleanup.
func NewFixture(t *testing.T) *Fixture {
	t.Helper()

	settings := map[string]any{
		"app": map[string]any{
			"env":  "test",
			"name": "claimy-integration",
			"tags": map[string]any{
				"project": "claimy",
				"family":  "integration",
			},
		},
		"test": map[string]any{
			"defaults": map[string]any{
				"images": map[string]any{
					mysqlImageName: map[string]any{"repository": mysqlImageName, "tag": "8.0.42"},
				},
			},
		},
		"sqlc": map[string]any{
			"default": map[string]any{
				"driver": mysqlImageName,
				"parameters": map[string]any{
					"loc":       "UTC",
					"time_zone": "'+00:00'",
				},
			},
		},
	}
	environment, err := env.NewEnvironment(t, env.WithConfigMap(settings))
	if err != nil {
		if environment != nil {
			if stopErr := environment.Stop(); stopErr != nil {
				t.Errorf("stop partially started MySQL environment: %v", stopErr)
			}
		}
		t.Fatalf("start disposable MySQL environment: %v", err)
	}

	fixture := &Fixture{}
	fixture.Context = environment.Context()
	fixture.Config = environment.Config()
	fixture.Logger = environment.Logger()
	fixture.SQLDB = environment.MySql("default").Client().DB
	t.Cleanup(func() {
		if fixture.Client != nil {
			if err := fixture.Client.Close(); err != nil {
				t.Errorf("close SQLC client: %v", err)
			}
		}
		if err := environment.MySql("default").Client().Close(); err != nil {
			t.Errorf("close disposable MySQL client: %v", err)
		}
		if err := environment.Stop(); err != nil {
			t.Errorf("stop disposable MySQL environment: %v", err)
		}
	})

	fixture.Client, err = sqlc.ProvideClient(fixture.Context, fixture.Config, fixture.Logger, "default")
	if err != nil {
		t.Fatalf("provide SQLC client for disposable MySQL: %v", err)
	}
	if err = applyMigrations(fixture.Context, fixture.SQLDB); err != nil {
		t.Fatalf("apply Claimy schema to disposable MySQL: %v", err)
	}

	return fixture
}

// NewDatabase is the compact fixture API used by store-only and domain tests.
// The fixture lifetime, migration and SQLC client are bound to t.Cleanup.
func NewDatabase(t *testing.T) sqlc.Client {
	t.Helper()

	return NewFixture(t).Client
}

func applyMigrations(ctx context.Context, db *sql.DB) error {
	migrationDirectory, err := locateMigrations()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, db, os.DirFS(migrationDirectory))
	if err != nil {
		return fmt.Errorf("create Goose provider: %w", err)
	}
	if _, err = provider.Up(ctx); err != nil {
		return fmt.Errorf("apply Goose migrations: %w", err)
	}

	return nil
}

func locateMigrations() (string, error) {
	var starts []string
	if _, source, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(source))
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		starts = append(starts, workingDirectory)
	}
	for _, start := range starts {
		for directory := start; ; directory = filepath.Dir(directory) {
			candidate := filepath.Join(directory, "build", "migrations", "claimy")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate, nil
			}
			parent := filepath.Dir(directory)
			if parent == directory {
				break
			}
		}
	}

	return "", fmt.Errorf("could not locate build/migrations/claimy from %v", starts)
}

// NewClient opens an independent SQLC pool against the fixture database.
func (f *Fixture) NewClient(t *testing.T) sqlc.Client {
	t.Helper()
	client, err := sqlc.NewClient(appctx.WithContainer(f.Context), f.Config, f.Logger, "default")
	if err != nil {
		t.Fatalf("create independent SQLC client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close independent SQLC client: %v", err)
		}
	})

	return client
}
