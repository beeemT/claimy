package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/beeemT/claimy/internal/claims"
)

func TestSlashCommandAcceptsGoogleInt64JSONEncodings(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int64
	}{
		{input: `{"commandId":"42"}`, want: 42},
		{input: `{"commandId":42}`, want: 42},
		{input: `{"commandId":"9223372036854775807"}`, want: 9223372036854775807},
		{input: `{"commandId":9223372036854775807}`, want: 9223372036854775807},
	} {
		var parsed slashCommand
		if err := json.Unmarshal([]byte(test.input), &parsed); err != nil {
			t.Fatalf("decode %s: %v", test.input, err)
		}
		if parsed.CommandID != test.want {
			t.Fatalf("commandId from %s = %d, want %d", test.input, parsed.CommandID, test.want)
		}
	}

	for _, input := range []string{
		`{}`,
		`{"commandId":"x"}`,
		`{"commandId":"9223372036854775808"}`,
		`{"commandId":42.5}`,
		`{"commandId":9223372036854775808}`,
	} {
		var parsed slashCommand
		if err := json.Unmarshal([]byte(input), &parsed); err == nil {
			t.Fatalf("decode %s succeeded, want invalid command id error", input)
		}
	}
}

func TestParseCommandCanonicalizesScopeAndPreservesDefaultExpiry(t *testing.T) {
	parsed, err := parseCommand("/claim take both Team app API", "")
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if parsed.kind != commandTake {
		t.Fatalf("kind = %v, want take", parsed.kind)
	}
	if parsed.scope != (claims.Scope{Group: "team", App: "api"}) {
		t.Fatalf("scope = %#v, want canonical group and app", parsed.scope)
	}
	if len(parsed.environments) != 2 || parsed.environments[0] != claims.Sandbox || parsed.environments[1] != claims.Prod {
		t.Fatalf("environments = %#v, want sandbox and prod", parsed.environments)
	}
	if parsed.expiresAt != nil {
		t.Fatalf("default expiry = %v, want nil for domain-time resolution after replay lookup", parsed.expiresAt)
	}
}

func TestParseCommandBerlinTimeAmbiguityAndExplicitOffset(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantError bool
		wantUTC   string
	}{
		{
			name:      "nonexistent spring local time",
			input:     "take sandbox platform until 2026-03-29 02:30",
			wantError: true,
		},
		{
			name:      "ambiguous autumn local time",
			input:     "take sandbox platform until 2026-10-25 02:30",
			wantError: true,
		},
		{
			name:    "explicit daylight offset resolves ambiguity",
			input:   "take prod platform until 2026-10-25T02:30:00+02:00",
			wantUTC: "2026-10-25T00:30:00Z",
		},
		{
			name:    "space-separated explicit offset resolves ambiguity",
			input:   "take prod platform until 2026-10-25 02:30+02:00",
			wantUTC: "2026-10-25T00:30:00Z",
		},
		{
			name:    "explicit standard offset resolves ambiguity",
			input:   "expiry 12345678-1234-1234-1234-123456789abc until 2026-10-25T02:30:00+01:00",
			wantUTC: "2026-10-25T01:30:00Z",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			assertBerlinCommandResult(t, test.input, test.wantError, test.wantUTC)
		})
	}
}

func assertBerlinCommandResult(t *testing.T, input string, wantError bool, wantUTC string) {
	t.Helper()

	parsed, err := parseCommand(input, "")
	if wantError {
		if err == nil {
			t.Fatal("parseCommand() succeeded for an invalid Berlin local time")
		}

		return
	}
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	var value *time.Time
	if parsed.kind == commandTake || parsed.kind == commandExpiry {
		value = parsed.expiresAt
	} else {
		value = parsed.at
	}
	if value == nil {
		t.Fatal("parsed command has no explicit time")
	}
	if got := value.UTC().Format(time.RFC3339); got != wantUTC {
		t.Fatalf("parsed UTC time = %s, want %s", got, wantUTC)
	}
}

func TestParseCommandRejectsMalformedGrammarAndNames(t *testing.T) {
	invalid := []string{
		"take staging platform",
		"take sandbox ../platform",
		"take prod platform app ../service",
		"take both platform app api until 2026-10-01 12:00 app worker",
		"free all platform",
		"free prod platform until 2026-10-01 12:00",
		"list staging",
		"list prod app api",
		"release not-a-claim-id",
		"expiry 12345678-1234-1234-1234-123456789abc at 2026-10-01 12:00",
		"/release 12345678-1234-1234-1234-123456789abc",
		"take sandbox platform\nfree prod platform",
	}
	for _, input := range invalid {
		t.Run(strings.ReplaceAll(input, " ", "_"), func(t *testing.T) {
			if parsed, err := parseCommand(input, ""); err == nil {
				t.Fatalf("parseCommand(%q) = %#v, want syntax error", input, parsed)
			}
		})
	}
}

func TestParseCommandSupportsChatArgumentTextAndOptionalListFilters(t *testing.T) {
	parsed, err := parseCommand("/claim", "take sandbox My-Group app My-App until 2026-10-06 17:30")
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if parsed.scope != (claims.Scope{Group: "my-group", App: "my-app"}) {
		t.Fatalf("scope = %#v", parsed.scope)
	}
	if parsed.expiresAt == nil || parsed.expiresAt.Location() != time.UTC {
		t.Fatalf("explicit parsed expiry = %v, want UTC time", parsed.expiresAt)
	}

	list, err := parseCommand("/claim", "list prod at 2026-10-06 17:30")
	if err != nil {
		t.Fatalf("parse list argumentText: %v", err)
	}
	if list.kind != commandList || len(list.environments) != 1 || list.environments[0] != claims.Prod || list.at == nil {
		t.Fatalf("list parse = %#v", list)
	}
	all, err := parseCommand("list", "")
	if err != nil {
		t.Fatalf("parse unfiltered list: %v", err)
	}
	if len(all.environments) != 0 || all.at != nil {
		t.Fatalf("unfiltered list = %#v, want no environment/time filter", all)
	}
}

func TestRequestIDUsesConfiguredAppSpaceAndMessageJSONTuple(t *testing.T) {
	handler := &Handler{appIdentity: "projects/example/agents/claimy"}
	parsed := command{kind: commandTake}
	got, err := handler.requestID(parsed, "spaces/AAAA", "spaces/AAAA/messages/123")
	if err != nil {
		t.Fatalf("requestID() error = %v", err)
	}
	payload, err := json.Marshal([3]string{"projects/example/agents/claimy", "spaces/AAAA", "spaces/AAAA/messages/123"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if want := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("request id = %q, want SHA-256 tuple %q", got, want)
	}
	if _, err = handler.requestID(parsed, "spaces/AAAA", ""); err == nil {
		t.Fatal("mutation without a Chat message identity was accepted")
	}
	if got, err = handler.requestID(command{kind: commandList}, "spaces/AAAA", ""); err != nil || got != "" {
		t.Fatalf("read-only command request id = %q, error = %v; want empty id", got, err)
	}
}

func TestBusyAndListRepliesIncludeClaimIdentityAndBerlinBoundaries(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{berlin: location}
	expires := time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC)
	busy := handler.formatBusy(command{
		scope:        claims.Scope{Group: "billing", App: "api"},
		environments: []claims.Environment{claims.Sandbox, claims.Prod},
	}, []claims.Conflict{{
		ID:           "12345678-1234-1234-1234-123456789abc",
		Scope:        claims.Scope{Group: "billing"},
		Environments: []claims.Environment{claims.Prod},
		OwnerEmail:   "alice@example.com",
		Source:       claims.CI,
		ExpiresAt:    expires,
	}})
	for _, part := range []string{"Busy:", "alice@example.com", "billing", "prod", "12345678-1234-1234-1234-123456789abc", "2026-10-25 02:30:00 CET"} {
		if !strings.Contains(busy, part) {
			t.Errorf("busy reply %q does not include %q", busy, part)
		}
	}

	listed := handler.formatList(claims.QueryResult{
		At:        time.Date(2026, time.October, 25, 1, 0, 0, 0, time.UTC),
		Projected: true,
		Claims: []claims.Claim{{
			ID:           "12345678-1234-1234-1234-123456789abc",
			Scope:        claims.Scope{Group: "billing", App: "api"},
			Environments: []claims.Environment{claims.Prod},
			OwnerEmail:   "alice@example.com",
			Source:       claims.CI,
			CreatedAt:    time.Date(2026, time.October, 24, 8, 0, 0, 0, time.UTC),
			ExpiresAt:    expires,
			Inherited:    true,
		}},
	})
	for _, part := range []string{"future projection, not a reservation", "alice@example.com", "started 2026-10-24 10:00:00 CEST", "expires 2026-10-25 02:30:00 CET", "source `ci`", "inherited group coverage"} {
		if !strings.Contains(listed, part) {
			t.Errorf("list reply %q does not include %q", listed, part)
		}
	}
}

func TestFreeReplySeparatesAvailabilityFromCallerPermission(t *testing.T) {
	handler := &Handler{berlin: time.UTC}
	free := handler.formatFree(command{}, claims.QueryResult{
		Known:            true,
		At:               time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		Projected:        true,
		Free:             false,
		AllowedForCaller: true,
	})
	if !strings.Contains(free, "Free: false") || !strings.Contains(free, "Allowed for you: true") || !strings.Contains(free, "future projection, not a reservation") {
		t.Fatalf("free reply does not distinguish availability, permission, and projection: %q", free)
	}
}

func TestClaimEnumFormattingDoesNotEchoUnknownValues(t *testing.T) {
	const maliciousEnvironment = "prod\n`unexpected`"
	if got := formatEnvironments([]claims.Environment{
		claims.Prod, claims.Environment(maliciousEnvironment), claims.Sandbox,
	}); got != "sandbox, prod, unknown" {
		t.Fatalf("formatted environments = %q, want only known labels", got)
	}
	if got := formatSource(claims.Source("ci\n`unexpected`")); got != "`unknown`" {
		t.Fatalf("formatted source = %q, want a safe unknown label", got)
	}
}

func TestInteractionContextHasConfiguredDeadline(t *testing.T) {
	const deadline = 5 * time.Millisecond
	started := time.Now()
	ctx, cancel := newInteractionContext(context.Background(), deadline)
	defer cancel()
	observed, ok := ctx.Deadline()
	if !ok || observed.After(started.Add(deadline+time.Second)) || observed.Before(started) {
		t.Fatalf("deadline = %v, present = %t; want bounded configured deadline", observed, ok)
	}
	<-ctx.Done()
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
	}
}

func TestBoundResponseKeepsUTF8AtLimit(t *testing.T) {
	input := strings.Repeat("a", maxResponseBytes-2) + "🌍" + strings.Repeat("b", 10)
	got := boundResponse(input)
	if !strings.Contains(got, "Results truncated to fit in a Chat reply.") {
		t.Fatalf("response was not truncated: len=%d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated response is not valid UTF-8")
	}
}

func TestOperationErrorsNeverEchoInternalDetails(t *testing.T) {
	const secret = "chat-bearer-token-must-not-appear"
	status, message := safeOperationError(&claims.Error{
		Code:    claims.StorageError,
		Message: "database failure carrying " + secret,
	})
	if status != 503 || strings.Contains(message, secret) {
		t.Fatalf("safe operation error = (%d, %q), leaked internal detail or wrong status", status, message)
	}
}
