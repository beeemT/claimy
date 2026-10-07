package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beeemT/claimy/internal/login"
)

func runAuth(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	jsonFlag := global.jsonSet
	cleanArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonFlag = true

			continue
		}
		cleanArgs = append(cleanArgs, arg)
	}
	_ = jsonFlag // Auth responses are always JSON.

	if len(cleanArgs) == 0 || isHelpFlag(cleanArgs[0]) {
		if err := writeAuthUsage(stderr); err != nil {
			return reportInputError(stderr, errors.New("could not write auth usage"))
		}
		if len(cleanArgs) == 0 {
			return 2
		}

		return 0
	}

	switch cleanArgs[0] {
	case "login":
		return runAuthLogin(ctx, global, cleanArgs[1:], stdout, stderr)
	case "logout":
		return runAuthLogout(global, cleanArgs[1:], stdout, stderr)
	default:
		return reportInputError(stderr, errors.New("unknown auth command"))
	}
}

func runAuthLogin(ctx context.Context, global globalOptions, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		if _, err := fmt.Fprintln(stderr, "Usage: claimy auth login URL"); err != nil {
			return 2
		}

		return 0
	}
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return reportInputError(stderr, errors.New("usage: claimy auth login URL"))
	}
	identity, err := login.Login(ctx, args[0], global.timeout, stderr)
	if err != nil {
		return reportInputError(stderr, err)
	}
	if identity == nil {
		return reportInputError(stderr, errors.New("login returned an empty identity"))
	}
	if err := writeJSON(stdout, identity); err != nil {
		return reportInputError(stderr, errors.New("could not write login response"))
	}

	return 0
}

func runAuthLogout(global globalOptions, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		if _, err := fmt.Fprintln(stderr, "Usage: claimy auth logout [URL]"); err != nil {
			return 2
		}

		return 0
	}
	if len(args) > 1 || (len(args) == 1 && strings.TrimSpace(args[0]) == "") {
		return reportInputError(stderr, errors.New("usage: claimy auth logout [URL]"))
	}
	server := ""

	switch {
	case len(args) == 1:
		server = args[0]
	case strings.TrimSpace(global.url) != "":
		server = global.url
	case global.urlSet:
		return reportInputError(stderr, errors.New("--url cannot be empty"))
	}
	if err := login.Logout(server); err != nil {
		return reportInputError(stderr, err)
	}
	if err := writeJSON(stdout, map[string]bool{"loggedOut": true}); err != nil {
		return reportInputError(stderr, errors.New("could not write logout response"))
	}

	return 0
}

func writeAuthUsage(stderr io.Writer) error {
	if _, err := fmt.Fprintln(stderr, "Usage: claimy auth <login URL|logout [URL]>"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stderr, "Use 'claimy auth login --help' or 'claimy auth logout --help' for details.")

	return err
}
