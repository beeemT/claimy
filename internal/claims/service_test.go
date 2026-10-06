package claims

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testActor() Actor {
	return Actor{Email: " Alice+Ops@Example.COM ", Issuer: "https://identity.example", Subject: "user-123", Channel: REST}
}

func mustDomainError(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var domainErr *Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if domainErr.Code != code {
		t.Fatalf("error code = %q, want %q", domainErr.Code, code)
	}
}

func TestCanonicalEmailPreservesAliases(t *testing.T) {
	got, err := CanonicalEmail("  Alice+Stage@Example.COM  ")
	if err != nil {
		t.Fatal(err)
	}
	if want := "alice+stage@example.com"; got != want {
		t.Fatalf("CanonicalEmail() = %q, want %q", got, want)
	}
	withoutAlias, err := CanonicalEmail("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got == withoutAlias {
		t.Fatal("plus-address alias was rewritten")
	}
	for _, invalid := range []string{"", "  ", "not-an-email", "display name <user@example.com>", "user@example.com\nattacker@example.com"} {
		if _, err := CanonicalEmail(invalid); err == nil {
			t.Errorf("CanonicalEmail(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestCanonicalSlugNormalizesCaseAndRejectsMalformedValues(t *testing.T) {
	got, err := CanonicalSlug("  My-Service-2  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-service-2" {
		t.Fatalf("CanonicalSlug() = %q, want my-service-2", got)
	}
	for _, invalid := range []string{"", "  ", "-leading", "trailing-", "double--hyphen", "under_score", "a/b", "café"} {
		if _, err := CanonicalSlug(invalid); err == nil {
			t.Errorf("CanonicalSlug(%q) unexpectedly succeeded", invalid)
		}
	}
	if _, err := CanonicalSlug(strings.Repeat("a", 129)); err == nil {
		t.Fatal("CanonicalSlug accepted a slug longer than 128 bytes")
	}
}

func TestDefaultExpiryUsesNextBerlinCalendarNoonAcrossDST(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "spring transition",
			now:  time.Date(2026, time.March, 28, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.March, 29, 10, 0, 0, 0, time.UTC),
		},
		{
			name: "fall transition",
			now:  time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.October, 25, 11, 0, 0, 0, time.UTC),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := DefaultExpiry(test.now)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) || got.Location() != time.UTC {
				t.Fatalf("DefaultExpiry() = %s (%s), want %s UTC", got, got.Location(), test.want)
			}
		})
	}
}

func TestParseExpiryRequiresRFC3339OffsetAndNormalizesMicroseconds(t *testing.T) {
	got, err := ParseExpiry("2026-04-05T14:20:30.123456789+02:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.April, 5, 12, 20, 30, 123456000, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC || got.Nanosecond() != 123456000 {
		t.Fatalf("ParseExpiry() = %s (%s), want %s UTC", got, got.Location(), want)
	}
	for _, invalid := range []string{
		"2026-04-05T14:20:30", "2026-04-05 14:20:30Z", "2026-04-05T14:20Z", "2026-04-05T14:20:30z", "not-a-time",
	} {
		if _, err := ParseExpiry(invalid); err == nil {
			t.Errorf("ParseExpiry(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestParseBerlinTimeRejectsDSTGapAndOverlapUnlessOffsetIsExplicit(t *testing.T) {
	for _, invalid := range []string{"2026-03-29 02:30", "2026-10-25 02:30", "2026-02-30 12:00", "2026-10-25 02:30:00 trailing"} {
		if _, err := ParseBerlinTime(invalid); err == nil {
			t.Errorf("ParseBerlinTime(%q) unexpectedly succeeded", invalid)
		}
	}
	valid := []struct {
		value string
		want  time.Time
	}{
		{"2026-03-29 03:30", time.Date(2026, time.March, 29, 1, 30, 0, 0, time.UTC)},
		{"2026-10-25 02:30+02:00", time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC)},
		{"2026-10-25 02:30+01:00", time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC)},
		{"2026-10-26T02:30:00+01:00", time.Date(2026, time.October, 26, 1, 30, 0, 0, time.UTC)},
		{"2026-10-26 02:30:00.123456789", time.Date(2026, time.October, 26, 1, 30, 0, 123456000, time.UTC)},
	}
	for _, test := range valid {
		got, err := ParseBerlinTime(test.value)
		if err != nil {
			t.Errorf("ParseBerlinTime(%q): %v", test.value, err)

			continue
		}
		if !got.Equal(test.want) || got.Location() != time.UTC {
			t.Errorf("ParseBerlinTime(%q) = %s (%s), want %s UTC", test.value, got, got.Location(), test.want)
		}
	}
}

func TestActorPrincipalUsesStablePerChannelKeys(t *testing.T) {
	cases := []struct {
		name   string
		actor  Actor
		kind   string
		issuer string
		wantID string
	}{
		{
			name:   "REST user",
			actor:  testActor(),
			kind:   "rest_user",
			issuer: "https://identity.example",
			wantID: "user-123",
		},
		{
			name: "Chat user",
			actor: Actor{
				Email: "chat.user@example.com", Issuer: "https://chat-issuer.example", Subject: "users/abc-123", Channel: GoogleChat,
			},
			kind:   "google_chat_user",
			issuer: "https://chat-issuer.example",
			wantID: "users/abc-123",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ActorPrincipal(test.actor)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.kind || got.Issuer != test.issuer || got.ID != test.wantID {
				t.Fatalf("ActorPrincipal() = %#v", got)
			}
		})
	}

	gitlabActor := Actor{
		Email: "ci@example.com", Issuer: "https://gitlab.example", Subject: "user-9", Channel: GitLabCI,
		GitLab: &GitLabIdentity{Issuer: "https://gitlab.example", ProjectID: "42", JobID: "9001", UserID: "user-9"},
	}
	got, err := ActorPrincipal(gitlabActor)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := json.Marshal([2]string{"42", "9001"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pair)
	if got.Kind != "gitlab_job" || got.Issuer != gitlabActor.GitLab.Issuer || got.ID != hex.EncodeToString(digest[:]) {
		t.Fatalf("GitLab ActorPrincipal() = %#v", got)
	}
	otherJob := gitlabActor
	otherJob.GitLab = &GitLabIdentity{Issuer: gitlabActor.GitLab.Issuer, ProjectID: "42", JobID: "9002", UserID: "user-9"}
	otherPrincipal, err := ActorPrincipal(otherJob)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == otherPrincipal.ID {
		t.Fatal("distinct GitLab jobs shared a principal ID")
	}
	sameJobDifferentUser := gitlabActor
	sameJobDifferentUser.Email = "renamed@example.com"
	sameJobDifferentUser.Subject = "user-10"
	sameJobDifferentUser.GitLab = &GitLabIdentity{Issuer: gitlabActor.GitLab.Issuer, ProjectID: "42", JobID: "9001", UserID: "user-10"}
	stablePrincipal, err := ActorPrincipal(sameJobDifferentUser)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != stablePrincipal.ID || got.Issuer != stablePrincipal.Issuer {
		t.Fatal("mutable GitLab user email or user ID changed the stable project/job principal")
	}
}

func TestActorPrincipalRejectsInconsistentAuthenticatedIdentity(t *testing.T) {
	validGitLab := Actor{
		Email: "ci@example.com", Issuer: "https://gitlab.example", Subject: "user-9", Channel: GitLabCI,
		GitLab: &GitLabIdentity{Issuer: "https://gitlab.example", ProjectID: "42", JobID: "9001", UserID: "user-9"},
	}
	cases := []Actor{
		{Email: "user@example.com", Issuer: "https://issuer.example", Subject: "u1", Channel: Channel("unknown")},
		{Email: "user@example.com", Issuer: "https://issuer.example", Subject: "u1", Channel: GitLabCI},
		{Email: "user@example.com", Issuer: "https://issuer.example", Subject: "u1", Channel: REST, GitLab: validGitLab.GitLab},
		{Email: "not-an-email", Issuer: "https://issuer.example", Subject: "u1", Channel: REST},
		{Email: "user@example.com", Issuer: "https://issuer.example", Subject: "user with spaces", Channel: REST},
	}
	mismatchedIssuer := validGitLab
	mismatchedIssuer.GitLab = &GitLabIdentity{Issuer: "https://another.example", ProjectID: "42", JobID: "9001", UserID: "user-9"}
	cases = append(cases, mismatchedIssuer)
	mismatchedSubject := validGitLab
	mismatchedSubject.Subject = "different-user"
	cases = append(cases, mismatchedSubject)
	for _, actor := range cases {
		if _, err := ActorPrincipal(actor); err == nil {
			t.Errorf("ActorPrincipal(%#v) unexpectedly succeeded", actor)
		} else {
			mustDomainError(t, err, Unauthenticated)
		}
	}
}
