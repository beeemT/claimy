package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/beeemT/claimy/pkg/client"
)

type errorTrackingWriter struct {
	io.Writer
	err error
}

func (writer *errorTrackingWriter) Write(data []byte) (int, error) {
	n, err := writer.Writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if writer.err == nil && err != nil {
		writer.err = err
	}

	return n, err
}

func newCommandFlagSet(name, usage string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	output := &errorTrackingWriter{Writer: stderr}
	flags.SetOutput(output)
	flags.Bool("json", false, "use JSON output (default)")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(output, "Usage: "+usage); err != nil {
			return
		}
		flags.PrintDefaults()
	}

	return flags
}

func parseCommandFlags(flags *flag.FlagSet, args []string) (bool, error) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if output, ok := flags.Output().(*errorTrackingWriter); ok && output.err != nil {
				return false, errors.New("could not write usage")
			}

			return true, nil
		}

		return false, err
	}
	if len(flags.Args()) != 0 {
		return false, errors.New("unexpected positional arguments")
	}

	return false, nil
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(item *flag.Flag) {
		if item.Name == name {
			set = true
		}
	})

	return set
}

func requireFlag(flags *flag.FlagSet, name, value string) error {
	if !flagWasSet(flags, name) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("--%s is required", name)
	}

	return nil
}

func requireUUIDFlag(flags *flag.FlagSet, name, value string) error {
	if err := requireFlag(flags, name, value); err != nil {
		return err
	}
	if !isUUID(value) {
		return fmt.Errorf("--%s must be a UUID", name)
	}

	return nil
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}

			continue
		}
		if !isHexDigit(character) {
			return false
		}
	}

	return true
}

func isHexDigit(character rune) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F'
}

func optionalApp(flags *flag.FlagSet, value string) (*string, error) {
	if !flagWasSet(flags, "app") {
		return nil, nil
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("--app must not be empty")
	}

	return &value, nil
}

func optionalCanonicalName(flags *flag.FlagSet, value string) (*string, error) {
	if !flagWasSet(flags, "canonical-name") {
		return nil, nil
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("--canonical-name must not be empty")
	}

	return &value, nil
}

func parseOptionalTime(flags *flag.FlagSet, name, value string) (*time.Time, error) {
	if !flagWasSet(flags, name) {
		return nil, nil
	}
	parsed, err := parseRFC3339(name, value)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func parseRFC3339(name, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--%s must be an RFC3339 time with an explicit offset", name)
	}

	return parsed, nil
}

func parseEnvironments(value string) ([]client.Environment, error) {
	if value == "both" {
		return []client.Environment{client.Environment("sandbox"), client.Environment("prod")}, nil
	}

	parts := strings.Split(value, ",")
	result := make([]client.Environment, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		environment := strings.TrimSpace(part)
		if environment != "sandbox" && environment != "prod" {
			return nil, errors.New("--environments must contain only sandbox and prod")
		}
		if _, ok := seen[environment]; ok {
			return nil, errors.New("--environments must not contain duplicates")
		}
		seen[environment] = struct{}{}
		result = append(result, client.Environment(environment))
	}

	return result, nil
}

func parseRevision(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, errors.New("--revision must be between 1 and 4294967295")
	}

	return uint32(parsed), nil
}

func parsePagination(limit, offset int) (int32, int32, error) {
	if limit < 1 || limit > defaultPageLimit {
		return 0, 0, errors.New("--limit must be between 1 and 100")
	}
	if offset < 0 || int64(offset) > 1<<31-1 {
		return 0, 0, errors.New("--offset must be between 0 and 2147483647")
	}

	return int32(limit), int32(offset), nil
}

func isHelpFlag(argument string) bool {
	return argument == "-h" || argument == "-help" || argument == "--help"
}
