package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/beeemT/claimy/pkg/client"
)

const outputJSON = "json"

func runAcquire(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("acquire", "claimy api [global flags] acquire --group GROUP --environments ENVIRONMENTS --request-id ID [flags]", stderr)
	group := flags.String("group", "", "application group (required)")
	app := flags.String("app", "", "application name")
	environments := flags.String("environments", "", "comma-separated sandbox,prod or both (required)")
	expiresAt := flags.String("expires-at", "", "optional expiry time in RFC3339 format")
	requestID := flags.String("request-id", "", "caller-provided idempotency key (required)")
	output := flags.String("output", outputJSON, "output format: json or id")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireFlag(flags, "group", *group); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "environments", *environments); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "request-id", *requestID); err != nil {
		return reportInputError(stderr, err)
	}
	if *output != outputJSON && *output != "id" {
		return reportInputError(stderr, errors.New("--output must be json or id"))
	}

	parsedEnvironments, err := parseEnvironments(*environments)
	if err != nil {
		return reportInputError(stderr, err)
	}
	appValue, err := optionalApp(flags, *app)
	if err != nil {
		return reportInputError(stderr, err)
	}
	parsedExpiry, err := parseOptionalTime(flags, "expires-at", *expiresAt)
	if err != nil {
		return reportInputError(stderr, err)
	}

	request := client.AcquireRequest{
		Group:        *group,
		App:          appValue,
		Environments: parsedEnvironments,
		ExpiresAt:    parsedExpiry,
		RequestId:    *requestID,
	}

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.Acquire(requestContext, request)
		if err != nil {
			return 2, err
		}

		return acquireResult(response, *output, stdout)
	})
}

func acquireResult(response *client.AcquireResponse, output string, stdout io.Writer) (int, error) {
	if response == nil {
		return 2, errors.New("server returned an empty acquire response")
	}
	if !response.Acquired {
		if response.Claim != nil || response.Conflicts == nil || len(*response.Conflicts) == 0 {
			return 2, errors.New("server returned an invalid busy response")
		}
		if output != outputJSON {
			return 1, nil
		}

		return writeAcquireResponse(response, stdout, 1)
	}
	if response.Claim == nil || (response.Conflicts != nil && len(*response.Conflicts) != 0) {
		return 2, errors.New("server returned an invalid acquired response")
	}
	if !response.Claim.ActiveNow {
		if output != outputJSON {
			return 1, nil
		}

		return writeAcquireResponse(response, stdout, 1)
	}

	id := response.Claim.Id.String()
	if output == "id" {
		if _, err := fmt.Fprintln(stdout, id); err != nil {
			return 2, fmt.Errorf("write claim ID: %w", err)
		}

		return 0, nil
	}

	return writeAcquireResponse(response, stdout, 0)
}

func writeAcquireResponse(response *client.AcquireResponse, stdout io.Writer, status int) (int, error) {
	if err := writeJSON(stdout, response); err != nil {
		return 2, fmt.Errorf("write acquire response: %w", err)
	}

	return status, nil
}

func runQuery(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("query", "claimy api [global flags] query --group GROUP [flags]", stderr)
	group := flags.String("group", "", "application group (required)")
	app := flags.String("app", "", "application name")
	environments := flags.String("environments", "both", "comma-separated sandbox,prod or both")
	at := flags.String("at", "", "optional query time in RFC3339 format")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireFlag(flags, "group", *group); err != nil {
		return reportInputError(stderr, err)
	}

	parsedEnvironments, err := parseEnvironments(*environments)
	if err != nil {
		return reportInputError(stderr, err)
	}
	appValue, err := optionalApp(flags, *app)
	if err != nil {
		return reportInputError(stderr, err)
	}
	parsedAt, err := parseOptionalTime(flags, "at", *at)
	if err != nil {
		return reportInputError(stderr, err)
	}
	environmentValues := parsedEnvironments
	request := client.QueryRequest{
		Group:        *group,
		App:          appValue,
		Environments: &environmentValues,
		At:           parsedAt,
	}

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.Query(requestContext, request)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty query response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write query response: %w", err)
		}

		return 0, nil
	})
}

func runRelease(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("release", "claimy api [global flags] release --id UUID --request-id ID", stderr)
	id := flags.String("id", "", "claim UUID (required)")
	requestID := flags.String("request-id", "", "caller-provided idempotency key (required)")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireUUIDFlag(flags, "id", *id); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "request-id", *requestID); err != nil {
		return reportInputError(stderr, err)
	}

	request := client.MutationRequest{RequestId: *requestID}

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.Release(requestContext, *id, request)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty release response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write release response: %w", err)
		}

		return 0, nil
	})
}

func runExpiry(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("expiry", "claimy api [global flags] expiry --id UUID --expires-at RFC3339 --revision REVISION --request-id ID", stderr)
	id := flags.String("id", "", "claim UUID (required)")
	expiresAt := flags.String("expires-at", "", "new expiry in RFC3339 format (required)")
	revision := flags.String("revision", "", "expected claim revision, 1..4294967295 (required)")
	requestID := flags.String("request-id", "", "caller-provided idempotency key (required)")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireUUIDFlag(flags, "id", *id); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "expires-at", *expiresAt); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "revision", *revision); err != nil {
		return reportInputError(stderr, err)
	}
	if err := requireFlag(flags, "request-id", *requestID); err != nil {
		return reportInputError(stderr, err)
	}

	parsedExpiry, err := parseRFC3339("expires-at", *expiresAt)
	if err != nil {
		return reportInputError(stderr, err)
	}
	parsedRevision, err := parseRevision(*revision)
	if err != nil {
		return reportInputError(stderr, err)
	}
	request := client.ExpiryRequest{
		ExpiresAt:        parsedExpiry,
		ExpectedRevision: int64(parsedRevision),
		RequestId:        *requestID,
	}

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.ChangeExpiry(requestContext, *id, request)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty expiry response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write expiry response: %w", err)
		}

		return 0, nil
	})
}

func runCatalog(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if err := writeCatalogUsage(stderr); err != nil {
			return reportInputError(stderr, errors.New("could not write catalog usage"))
		}

		return 2
	}
	if isHelpFlag(args[0]) {
		if err := writeCatalogUsage(stderr); err != nil {
			return reportInputError(stderr, errors.New("could not write catalog usage"))
		}

		return 0
	}

	switch args[0] {
	case "groups":
		return runCatalogGroups(ctx, global, args[1:], stdout, stderr)
	case "group":
		return runCatalogGroup(ctx, global, args[1:], stdout, stderr)
	case "apps":
		return runCatalogApps(ctx, global, args[1:], stdout, stderr)
	default:
		return reportInputError(stderr, errors.New("unknown catalog operation"))
	}
}

func runCatalogGroups(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	canonicalName, limit, offset, help, err := parseCatalogListFlags("catalog groups", args, stderr)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}

	request := catalogListRequestValues(canonicalName, limit, offset)

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.ListGroups(requestContext, request)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty catalog groups response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write catalog groups response: %w", err)
		}

		return 0, nil
	})
}

func runCatalogGroup(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("catalog group", "claimy api [global flags] catalog group --group GROUP", stderr)
	group := flags.String("group", "", "canonical application group name (required)")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireFlag(flags, "group", *group); err != nil {
		return reportInputError(stderr, err)
	}

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.GetGroup(requestContext, *group)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty catalog group response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write catalog group response: %w", err)
		}

		return 0, nil
	})
}

func runCatalogApps(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("catalog apps", "claimy api [global flags] catalog apps --group GROUP [flags]", stderr)
	group := flags.String("group", "", "canonical application group name (required)")
	canonicalName := flags.String("canonical-name", "", "optional canonical app name filter")
	limit := flags.Int("limit", defaultPageLimit, "page size, 1..100")
	offset := flags.Int("offset", 0, "zero-based result offset")

	help, err := parseCommandFlags(flags, args)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if help {
		return 0
	}
	if err := requireFlag(flags, "group", *group); err != nil {
		return reportInputError(stderr, err)
	}
	filter, err := optionalCanonicalName(flags, *canonicalName)
	if err != nil {
		return reportInputError(stderr, err)
	}
	pageLimit, pageOffset, err := parsePagination(*limit, *offset)
	if err != nil {
		return reportInputError(stderr, err)
	}
	request := catalogListRequestValues(filter, pageLimit, pageOffset)

	return perform(ctx, global, stderr, func(requestContext context.Context, api *client.API) (int, error) {
		response, err := api.ListApps(requestContext, *group, request)
		if err != nil {
			return 2, err
		}
		if response == nil {
			return 2, errors.New("server returned an empty catalog apps response")
		}
		if err := writeJSON(stdout, response); err != nil {
			return 2, fmt.Errorf("write catalog apps response: %w", err)
		}

		return 0, nil
	})
}

func parseCatalogListFlags(name string, args []string, stderr io.Writer) (*string, int32, int32, bool, error) {
	flags := newCommandFlagSet(name, "claimy api [global flags] "+name+" [flags]", stderr)
	canonicalName := flags.String("canonical-name", "", "optional canonical name filter")
	limit := flags.Int("limit", defaultPageLimit, "page size, 1..100")
	offset := flags.Int("offset", 0, "zero-based result offset")

	help, err := parseCommandFlags(flags, args)
	if err != nil || help {
		return nil, 0, 0, help, err
	}

	filter, err := optionalCanonicalName(flags, *canonicalName)
	if err != nil {
		return nil, 0, 0, false, err
	}
	pageLimit, pageOffset, err := parsePagination(*limit, *offset)
	if err != nil {
		return nil, 0, 0, false, err
	}

	return filter, pageLimit, pageOffset, false, nil
}

func catalogListRequestValues(canonicalName *string, limit, offset int32) client.CatalogListRequest {
	page := &client.CatalogPage{Limit: &limit, Offset: &offset}
	request := client.CatalogListRequest{Page: page}
	if canonicalName != nil {
		request.Filter = &client.CatalogFilter{CanonicalName: canonicalName}
	}

	return request
}

func writeCatalogUsage(stderr io.Writer) error {
	if _, err := fmt.Fprintln(stderr, "Usage: claimy api [global flags] catalog <groups|group|apps> [flags]"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stderr, "Use 'claimy api catalog <groups|group|apps> --help' for catalog flags.")

	return err
}
