package chat

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/beeemT/claimy/internal/claims"
)

type commandKind uint8

const (
	commandTake commandKind = iota + 1
	commandFree
	commandList
	commandRelease
	commandExpiry
)

type command struct {
	kind         commandKind
	scope        claims.Scope
	environments []claims.Environment
	expiresAt    *time.Time
	at           *time.Time
	claimID      string
}

var claimIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseCommand(text string, argumentText string) (command, error) {
	input := strings.TrimSpace(argumentText)
	if input == "" {
		input = strings.TrimSpace(text)
	}
	if input == "" || strings.ContainsAny(input, "\r\n\x00") {
		return command{}, errCommandSyntax
	}
	for _, r := range input {
		if unicode.IsControl(r) {
			return command{}, errCommandSyntax
		}
	}

	fields := strings.Fields(input)
	if len(fields) == 0 {
		return command{}, errCommandSyntax
	}
	if fields[0] == "/claim" {
		fields = fields[1:]
	} else if strings.HasPrefix(fields[0], "/") {
		return command{}, errCommandSyntax
	}
	if len(fields) == 0 {
		return command{}, errCommandSyntax
	}

	switch fields[0] {
	case "take":
		return parseTake(fields)
	case "free":
		return parseFree(fields)
	case "list":
		return parseList(fields)
	case "release":
		return parseRelease(fields)
	case "expiry":
		return parseExpiry(fields)
	default:
		return command{}, errCommandSyntax
	}
}

var errCommandSyntax = errors.New("invalid command syntax")

func parseTake(fields []string) (command, error) {
	if len(fields) < 3 {
		return command{}, errCommandSyntax
	}
	environments, ok := parseEnvironments(fields[1])
	if !ok {
		return command{}, errCommandSyntax
	}
	group, err := claims.CanonicalSlug(fields[2])
	if err != nil {
		return command{}, errCommandSyntax
	}
	parsed := command{kind: commandTake, scope: claims.Scope{Group: group}, environments: environments}
	position := 3
	if position < len(fields) && fields[position] == "app" {
		if position+1 >= len(fields) {
			return command{}, errCommandSyntax
		}
		app, slugErr := claims.CanonicalSlug(fields[position+1])
		if slugErr != nil {
			return command{}, errCommandSyntax
		}
		parsed.scope.App = app
		position += 2
	}
	if position < len(fields) && fields[position] == "until" {
		parsedTime, timeErr := parseBerlinTimeFields(fields[position+1:])
		if timeErr != nil {
			return command{}, errCommandSyntax
		}
		parsed.expiresAt = &parsedTime
		position = len(fields)
	}
	if position != len(fields) {
		return command{}, errCommandSyntax
	}

	return parsed, nil
}

func parseFree(fields []string) (command, error) {
	if len(fields) < 3 {
		return command{}, errCommandSyntax
	}
	environments, ok := parseEnvironments(fields[1])
	if !ok {
		return command{}, errCommandSyntax
	}
	group, err := claims.CanonicalSlug(fields[2])
	if err != nil {
		return command{}, errCommandSyntax
	}
	parsed := command{kind: commandFree, scope: claims.Scope{Group: group}, environments: environments}
	position := 3
	if position < len(fields) && fields[position] == "app" {
		if position+1 >= len(fields) {
			return command{}, errCommandSyntax
		}
		app, slugErr := claims.CanonicalSlug(fields[position+1])
		if slugErr != nil {
			return command{}, errCommandSyntax
		}
		parsed.scope.App = app
		position += 2
	}
	if position < len(fields) && fields[position] == "at" {
		parsedTime, timeErr := parseBerlinTimeFields(fields[position+1:])
		if timeErr != nil {
			return command{}, errCommandSyntax
		}
		parsed.at = &parsedTime
		position = len(fields)
	}
	if position != len(fields) {
		return command{}, errCommandSyntax
	}

	return parsed, nil
}

func parseList(fields []string) (command, error) {
	parsed := command{kind: commandList}
	position := 1
	if position < len(fields) {
		environments, ok := parseEnvironments(fields[position])
		if ok {
			parsed.environments = environments
			position++
		}
	}
	if position < len(fields) && fields[position] == "at" {
		parsedTime, err := parseBerlinTimeFields(fields[position+1:])
		if err != nil {
			return command{}, errCommandSyntax
		}
		parsed.at = &parsedTime
		position = len(fields)
	}
	if position != len(fields) {
		return command{}, errCommandSyntax
	}

	return parsed, nil
}

func parseRelease(fields []string) (command, error) {
	if len(fields) != 2 || !claimIDPattern.MatchString(fields[1]) {
		return command{}, errCommandSyntax
	}

	return command{kind: commandRelease, claimID: strings.ToLower(fields[1])}, nil
}

func parseExpiry(fields []string) (command, error) {
	if len(fields) < 4 || !claimIDPattern.MatchString(fields[1]) || fields[2] != "until" {
		return command{}, errCommandSyntax
	}
	expiresAt, err := parseBerlinTimeFields(fields[3:])
	if err != nil {
		return command{}, errCommandSyntax
	}

	return command{kind: commandExpiry, claimID: strings.ToLower(fields[1]), expiresAt: &expiresAt}, nil
}

func parseEnvironments(value string) ([]claims.Environment, bool) {
	switch value {
	case "sandbox":
		return []claims.Environment{claims.Sandbox}, true
	case "prod":
		return []claims.Environment{claims.Prod}, true
	case "both":
		return []claims.Environment{claims.Sandbox, claims.Prod}, true
	default:
		return nil, false
	}
}

func parseBerlinTimeFields(fields []string) (time.Time, error) {
	if len(fields) == 0 {
		return time.Time{}, errCommandSyntax
	}

	return claims.ParseBerlinTime(strings.Join(fields, " "))
}
