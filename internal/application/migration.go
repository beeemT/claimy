package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
)

const (
	migrationConfigFile = "config.dist.yml"
	gooseBinaryPath     = "/app/goose"
	gooseMigrationPath  = "/app/build/migrations/claimy"
)

// NewMigrationConfig loads the same config file and environment overrides as the HTTP application.
func NewMigrationConfig() (cfg.Config, error) {
	config := cfg.New(map[string]any{
		"app": map[string]any{
			"env":  "dev",
			"name": "gosoline",
		},
	})
	if err := config.Option(
		cfg.WithEnvKeyReplacer(cfg.DefaultEnvKeyReplacer),
		cfg.WithSanitizers(cfg.TimeSanitizer),
		cfg.WithConfigFile(migrationConfigFile, "yml"),
	); err != nil {
		return nil, fmt.Errorf("load migration configuration: %w", err)
	}

	return config, nil
}

// RunMigration applies the embedded Claimy migrations using the effective SQLC settings.
func RunMigration(ctx context.Context, config cfg.Config) int {
	settings, err := effectiveDatabaseSettings(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read migration database settings: %v\n", err)

		return 1
	}
	if settings.Driver != sqlc.DriverMysql {
		fmt.Fprintf(os.Stderr, "Claimy migrations require the %s SQLC driver\n", sqlc.DriverMysql)

		return 1
	}

	driver, err := sqlc.GetDriver(log.NewLogger(), settings.Driver)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize migration database driver: %v\n", err)

		return 1
	}
	dsn := driver.GetDSN(settings)
	command := exec.CommandContext(ctx, gooseBinaryPath, "-env", "none", "-dir", gooseMigrationPath, "up")
	childEnv := os.Environ()
	filteredEnv := childEnv[:0]
	for _, entry := range childEnv {
		if !strings.HasPrefix(entry, "CLAIMY_DATABASE_PASSWORD=") {
			filteredEnv = append(filteredEnv, entry)
		}
	}
	clear(childEnv[len(filteredEnv):])
	filteredEnv = append(filteredEnv, "GOOSE_DRIVER="+sqlc.DriverMysql, "GOOSE_DBSTRING="+dsn)
	command.Env = filteredEnv

	var stdoutBuffer, stderrBuffer bytes.Buffer
	command.Stdout = &stdoutBuffer
	command.Stderr = &stderrBuffer
	runErr := command.Run()

	stdoutErr := writeRedactedGooseOutput(os.Stdout, stdoutBuffer.Bytes(), dsn, settings.Uri.Password)
	stderrErr := writeRedactedGooseOutput(os.Stderr, stderrBuffer.Bytes(), dsn, settings.Uri.Password)
	if stdoutErr != nil || stderrErr != nil {
		fmt.Fprintln(os.Stderr, "write Goose migration output failed")

		return 1
	}
	if runErr == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "Goose migration canceled: %v\n", ctx.Err())

		return 1
	}
	fmt.Fprintf(os.Stderr, "run Goose migrations: %v\n", runErr)

	return 1
}

func writeRedactedGooseOutput(destination io.Writer, output []byte, dsn, password string) error {
	if len(output) == 0 {
		return nil
	}
	message := string(output)
	if dsn != "" {
		message = strings.ReplaceAll(message, strconv.Quote(dsn), "[redacted database DSN]")
		message = strings.ReplaceAll(message, dsn, "[redacted database DSN]")
	}
	if password != "" {
		message = strings.ReplaceAll(message, strconv.Quote(password), "[redacted]")
		message = strings.ReplaceAll(message, password, "[redacted]")
	}
	_, err := io.WriteString(destination, message)

	return err
}
