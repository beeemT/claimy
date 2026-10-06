// Package cli implements Claimy's public API command-line client.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/beeemT/claimy/pkg/client"
)

const (
	claimyURLKey     = "CLAIMY_URL"
	claimyTokenKey   = "CLAIMY_ID_TOKEN"
	defaultTimeout   = 30 * time.Second
	defaultPageLimit = 100
)

type globalOptions struct {
	url          string
	tokenFile    string
	tokenFileSet bool
	timeout      time.Duration
}

// Run executes the API CLI arguments and returns the process exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, commandArgs, help, err := parseGlobalFlags(args, stderr)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if len(commandArgs) == 0 {
		if err := writeRootUsage(stderr); err != nil {
			return reportInputError(stderr, errors.New("could not write usage"))
		}

		return 2
	}

	switch commandArgs[0] {
	case "acquire":
		return runAcquire(ctx, options, commandArgs[1:], stdout, stderr)
	case "query":
		return runQuery(ctx, options, commandArgs[1:], stdout, stderr)
	case "release":
		return runRelease(ctx, options, commandArgs[1:], stdout, stderr)
	case "expiry":
		return runExpiry(ctx, options, commandArgs[1:], stdout, stderr)
	case "catalog":
		return runCatalog(ctx, options, commandArgs[1:], stdout, stderr)
	default:
		return reportInputError(stderr, errors.New("unknown API operation"))
	}
}

func parseGlobalFlags(args []string, stderr io.Writer) (globalOptions, []string, bool, error) {
	flags := flag.NewFlagSet("claimy api", flag.ContinueOnError)
	flags.SetOutput(stderr)

	options := globalOptions{}
	flags.StringVar(&options.url, "url", os.Getenv(claimyURLKey), "API base URL (defaults to CLAIMY_URL)")
	flags.StringVar(&options.tokenFile, "token-file", "", "read the bearer token from a file (overrides CLAIMY_ID_TOKEN)")
	flags.Lookup("url").DefValue = ""
	flags.DurationVar(&options.timeout, "timeout", defaultTimeout, "maximum duration for the API request")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "Usage: claimy api [global flags] <acquire|query|release|expiry|catalog>"); err != nil {
			return
		}
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options, nil, true, nil
		}

		return options, nil, false, err
	}
	options.tokenFileSet = flagWasSet(flags, "token-file")
	if options.timeout <= 0 {
		return options, nil, false, errors.New("--timeout must be positive")
	}

	return options, flags.Args(), false, nil
}

func writeRootUsage(stderr io.Writer) error {
	if _, err := fmt.Fprintln(stderr, "Usage: claimy api [global flags] <acquire|query|release|expiry|catalog>"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stderr, "Global flags: --url, --token-file, --timeout"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stderr, "Use 'claimy api --help' for global options and 'claimy api <operation> --help' for operation flags.")

	return err
}

func (options globalOptions) newAPI() (*client.API, string, error) {
	if strings.TrimSpace(options.url) == "" {
		return nil, "", errors.New("--url or CLAIMY_URL is required")
	}

	token, err := loadToken(options.tokenFile, options.tokenFileSet)
	if err != nil {
		return nil, "", err
	}

	api, err := client.New(options.url, client.WithBearerToken(token), client.WithHTTPClient(&http.Client{Timeout: options.timeout}))
	if err != nil {
		return nil, token, err
	}

	return api, token, nil
}

func loadToken(tokenFile string, tokenFileSet bool) (string, error) {
	if tokenFileSet {
		tokenBytes, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", errors.New("could not read bearer token file")
		}
		token := strings.TrimSpace(string(tokenBytes))
		if token == "" {
			return "", errors.New("bearer token file is empty")
		}

		return token, nil
	}

	token := strings.TrimSpace(os.Getenv(claimyTokenKey))
	if token == "" {
		return "", errors.New("--token-file or CLAIMY_ID_TOKEN is required")
	}

	return token, nil
}

func perform(ctx context.Context, options globalOptions, stderr io.Writer, operation func(context.Context, *client.API) (int, error)) int {
	api, token, err := options.newAPI()
	if err != nil {
		return reportFailure(stderr, token, err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	status, err := operation(requestContext, api)
	if err != nil {
		return reportFailure(stderr, token, err)
	}

	return status
}

func reportFailure(stderr io.Writer, token string, err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		message := redact(apiErr.Message, token)
		if message == "" {
			message = "request failed"
		}
		if _, err := fmt.Fprintf(stderr, "claimy api: HTTP %d (%v): %s\n", apiErr.StatusCode, apiErr.Code, message); err != nil {
			return 2
		}

		return 2
	}

	if _, writeErr := fmt.Fprintf(stderr, "claimy api: %s\n", redact(err.Error(), token)); writeErr != nil {
		return 2
	}

	return 2
}

func reportInputError(stderr io.Writer, err error) int {
	if _, writeErr := fmt.Fprintf(stderr, "claimy api: %s\n", err.Error()); writeErr != nil {
		return 2
	}

	return 2
}

func redact(message, token string) string {
	if token == "" {
		return message
	}

	return strings.ReplaceAll(message, token, "[redacted]")
}

func writeJSON(stdout io.Writer, value any) error {
	return json.NewEncoder(stdout).Encode(value)
}
