// Package application connects Claimy adapters and database lifecycle modules.
package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/beeemT/claimy/internal/api"
	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/chat"
	"github.com/beeemT/claimy/internal/claims"
	storemysql "github.com/beeemT/claimy/internal/storage/mysql"
	"github.com/gin-gonic/gin"
	"github.com/gosoline-project/httpserver"
	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/appctx"
	gosoapp "github.com/justtrackio/gosoline/pkg/application"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/kernel"
	"github.com/justtrackio/gosoline/pkg/log"
)

// Settings configures the identity adapters.
type Settings struct {
	Auth auth.Settings `cfg:"auth"`
	Chat chat.Settings `cfg:"chat"`
}

// Runtime shares the initialized adapters and SQLC client.
type Runtime struct {
	Client     sqlc.Client
	Database   *sql.DB
	Store      *storemysql.Repository
	Service    *claims.Service
	Identities *auth.Verifier
	Chat       *chat.Handler
}

type runtimeKey struct{}

// CLAIMY_DATABASE_PASSWORD is read raw to avoid Goso interpolation of Secret values.
const databasePasswordEnv = "CLAIMY_DATABASE_PASSWORD"

// Provide initializes one runtime for the application context.
func Provide(ctx context.Context, config cfg.Config, logger log.Logger) (*Runtime, error) {
	return appctx.Provide(ctx, runtimeKey{}, func() (*Runtime, error) {
		var settings Settings
		if err := config.UnmarshalKey("claimy", &settings); err != nil {
			return nil, fmt.Errorf("read claim service settings: %w", err)
		}
		database, client, err := provideDatabaseAndClient(ctx, config, logger)
		if err != nil {
			return nil, err
		}
		fail := func(cause error) (*Runtime, error) {
			return nil, errors.Join(cause, client.Close())
		}
		if err = verifySchema(ctx, client); err != nil {
			return fail(err)
		}
		identities, err := auth.New(settings.Auth)
		if err != nil {
			return fail(fmt.Errorf("initialize identity verification: %w", err))
		}
		store := storemysql.New(client)
		service := claims.NewService(store)
		chatHandler, err := chat.New(service, identities, settings.Chat)
		if err != nil {
			return fail(fmt.Errorf("initialize Chat adapter: %w", err))
		}

		return &Runtime{Client: client, Database: database.SQLDB(), Store: store, Service: service, Identities: identities, Chat: chatHandler}, nil
	})
}

func provideDatabaseAndClient(ctx context.Context, config cfg.Config, logger log.Logger) (*sqlc.DB, sqlc.Client, error) {
	databasePassword, hasDatabasePassword := os.LookupEnv(databasePasswordEnv)
	var (
		database         *sqlc.DB
		databaseSettings *sqlc.Settings
		err              error
	)
	if hasDatabasePassword {
		databaseSettings, err = sqlc.ReadSettings(config, "default")
		if err == nil {
			databaseSettings.Uri.Password = databasePassword
			database, err = sqlc.ProvideDBFromSettings(ctx, logger, "default", databaseSettings)
		}
	} else {
		database, err = sqlc.ProvideDB(ctx, config, logger, "default")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("initialize claim database: %w", err)
	}
	if err = verifyServer(ctx, database.SQLDB()); err != nil {
		return nil, nil, errors.Join(err, database.Close())
	}

	var client sqlc.Client
	if hasDatabasePassword {
		client, err = sqlc.NewClientWithSettings(ctx, config, logger, "default", databaseSettings)
	} else {
		client, err = sqlc.ProvideClient(ctx, config, logger, "default")
	}
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("initialize claim database: %w", err), database.Close())
	}

	return database, client, nil
}

func verifyServer(ctx context.Context, database *sql.DB) error {
	var version string
	if err := database.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return fmt.Errorf("read MySQL version: %w", err)
	}
	if err := verifyMySQLVersion(version); err != nil {
		return err
	}
	var sessionZone string
	if err := database.QueryRowContext(ctx, "SELECT @@session.time_zone").Scan(&sessionZone); err != nil {
		return fmt.Errorf("read database session timezone: %w", err)
	}
	if sessionZone != "+00:00" {
		return fmt.Errorf("database session timezone must be +00:00")
	}

	return nil
}

func verifyMySQLVersion(version string) error {
	actual := strings.SplitN(version, "-", 2)[0]
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return fmt.Errorf("MariaDB is not supported")
	}
	var major, minor, patch int
	if n, err := fmt.Sscanf(actual, "%d.%d.%d", &major, &minor, &patch); err != nil || n != 3 ||
		major < 8 || (major == 8 && minor == 0 && patch < 16) {
		return fmt.Errorf("MySQL 8.0.16 or later is required for enforced CHECK constraints")
	}

	return nil
}

func verifySchema(ctx context.Context, client sqlc.Client) error {
	var tables int
	query := "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND engine = 'InnoDB' AND table_name IN ('app_groups','apps','claims','claim_environments','claim_versions','request_results')"
	if err := client.Get(ctx, &tables, query); err != nil {
		return fmt.Errorf("verify claim schema: %w", err)
	}
	if tables != 6 {
		return fmt.Errorf("claim schema is incomplete; apply the Goose migrations before starting the service")
	}

	return nil
}

// Register installs the authenticated REST and Chat routes.
func Register(ctx context.Context, config cfg.Config, logger log.Logger, router *httpserver.Router) error {
	runtime, err := Provide(ctx, config, logger)
	if err != nil {
		return err
	}
	router.GET("/ready", ready(runtime.Database))
	if err = api.Register(ctx, config, logger, router, runtime.Service, runtime.Identities, runtime.Client); err != nil {
		return err
	}
	chat.Register(router, runtime.Chat)

	return nil
}

func ready(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			c.Status(http.StatusServiceUnavailable)

			return
		}
		c.Status(http.StatusOK)
	}
}

// Options keeps the database open until the HTTP application has drained.
func Options() []gosoapp.Option {
	return []gosoapp.Option{
		gosoapp.WithModuleFactory("claimy-database", databaseLifetime, kernel.ModuleType(kernel.TypeBackground), kernel.ModuleStage(kernel.StageEssential)),
		gosoapp.WithModuleFactory("claimy-retention", retention, kernel.ModuleType(kernel.TypeBackground), kernel.ModuleStage(kernel.StageApplication)),
	}
}

func databaseLifetime(ctx context.Context, config cfg.Config, logger log.Logger) (kernel.Module, error) {
	runtime, err := Provide(ctx, config, logger)
	if err != nil {
		return nil, err
	}

	return kernel.NewModuleFunc(func(ctx context.Context) error {
		<-ctx.Done()

		return runtime.Client.Close()
	}), nil
}

func retention(ctx context.Context, config cfg.Config, logger log.Logger) (kernel.Module, error) {
	runtime, err := Provide(ctx, config, logger)
	if err != nil {
		return nil, err
	}

	return kernel.NewModuleFunc(func(ctx context.Context) error {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			if _, err := runtime.Store.Prune(ctx); err != nil && ctx.Err() == nil {
				logger.Warn(ctx, "claim retention failed: %s", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}), nil
}
