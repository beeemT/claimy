// Package claims defines the claim domain and its canonical rules.
package claims

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"
	// Include IANA time zone data for deployments without system zone files.
	_ "time/tzdata"
)

const (
	maxSlugLength      = 128
	maxEmailLength     = 320
	maxIssuerLength    = 255
	maxSubjectLength   = 128
	maxRequestIDLength = 128
)

var berlinZoneCache struct {
	once sync.Once
	loc  *time.Location
	err  error
}

func berlinLocation() (*time.Location, error) {
	berlinZoneCache.once.Do(func() {
		berlinZoneCache.loc, berlinZoneCache.err = time.LoadLocation("Europe/Berlin")
	})

	return berlinZoneCache.loc, berlinZoneCache.err
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// CanonicalEmail trims surrounding whitespace and lowercases an email address without rewriting aliases.
func CanonicalEmail(value string) (string, error) {
	canonical := strings.ToLower(strings.TrimSpace(value))
	if canonical == "" || len(canonical) > maxEmailLength {
		return "", invalid("email is required and must be at most 320 bytes")
	}
	address, err := mail.ParseAddress(canonical)
	if err != nil || address.Name != "" || address.Address != canonical {
		return "", invalid("email address is invalid")
	}

	return canonical, nil
}

// CanonicalSlug trims and lowercases a technical slug, rejecting non-ASCII or malformed values.
func CanonicalSlug(value string) (string, error) {
	canonical := strings.ToLower(strings.TrimSpace(value))
	if len(canonical) == 0 || len(canonical) > maxSlugLength || !slugPattern.MatchString(canonical) {
		return "", invalid("slug must contain 1 to 128 ASCII letters, digits, or single hyphens")
	}

	return canonical, nil
}

// DefaultExpiry returns 12:00 Europe/Berlin on the calendar day after now.
func DefaultExpiry(now time.Time) (time.Time, error) {
	location, err := berlinLocation()
	if err != nil {
		return time.Time{}, fmt.Errorf("load Europe/Berlin time zone: %w", err)
	}
	local := now.In(location)
	nextNoon := time.Date(local.Year(), local.Month(), local.Day()+1, 12, 0, 0, 0, location)

	return canonicalTime(nextNoon), nil
}

// ParseExpiry parses one RFC3339 timestamp with an explicit UTC designator or numeric offset.
func ParseExpiry(value string) (time.Time, error) {
	if !hasRFC3339Offset(value) {
		return time.Time{}, invalid("expiry must be RFC3339 with an explicit offset")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, invalid("expiry must be RFC3339 with an explicit offset")
	}

	return canonicalTime(parsed), nil
}

// ParseBerlinTime parses an exact Berlin wall time or an explicitly offset timestamp.
// Local forms are YYYY-MM-DD HH:MM[:SS[.fraction]]; offset forms may use RFC3339 or
// the same space-separated form with a numeric offset.
func ParseBerlinTime(value string) (time.Time, error) {
	if hasSpaceSeparatedOffset(value) {
		for _, layout := range []string{
			"2006-01-02 15:04Z07:00",
			"2006-01-02 15:04:05Z07:00",
			"2006-01-02 15:04:05.999999999Z07:00",
		} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return canonicalTime(parsed), nil
			}
		}

		return time.Time{}, invalid("expiry must be a Berlin local time or an explicit-offset timestamp")
	}
	if hasRFC3339Offset(value) {
		return ParseExpiry(value)
	}

	var wall time.Time
	parsed := false
	for _, layout := range []string{
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
	} {
		candidate, err := time.Parse(layout, value)
		if err == nil {
			wall = candidate.UTC()
			parsed = true

			break
		}
	}
	if !parsed {
		return time.Time{}, invalid("expiry must use YYYY-MM-DD HH:MM[:SS[.fraction]] or an explicit offset")
	}

	location, err := berlinLocation()
	if err != nil {
		return time.Time{}, fmt.Errorf("load Europe/Berlin time zone: %w", err)
	}
	candidates := berlinWallTimeCandidates(wall, location)
	switch len(candidates) {
	case 0:
		return time.Time{}, invalid("Berlin local time does not exist; provide an explicit offset")
	case 1:
		return canonicalTime(candidates[0]), nil
	default:
		return time.Time{}, invalid("Berlin local time is ambiguous; provide an explicit offset")
	}
}

// ActorPrincipal returns the stable idempotency key for a validated authenticated actor.
func ActorPrincipal(actor Actor) (Principal, error) {
	canonical, err := canonicalActor(actor)
	if err != nil {
		return Principal{}, err
	}

	return principalForActor(canonical)
}

func canonicalActor(actor Actor) (Actor, error) {
	email, err := CanonicalEmail(actor.Email)
	if err != nil {
		return Actor{}, unauthenticated("authenticated actor identity is invalid")
	}
	if !validOpaqueASCII(actor.Issuer, maxIssuerLength) || !validOpaqueASCII(actor.Subject, maxSubjectLength) {
		return Actor{}, unauthenticated("authenticated actor identity is invalid")
	}
	actor.Email = email

	switch actor.Channel {
	case REST, GoogleChat:
		if actor.GitLab != nil {
			return Actor{}, unauthenticated("authenticated actor identity is invalid")
		}
	case GitLabCI:
		if actor.GitLab == nil || !validOpaqueASCII(actor.GitLab.Issuer, maxIssuerLength) ||
			actor.GitLab.Issuer != actor.Issuer || !validOpaqueASCII(actor.GitLab.ProjectID, maxSubjectLength) ||
			!validOpaqueASCII(actor.GitLab.JobID, maxSubjectLength) || !validOpaqueASCII(actor.GitLab.UserID, maxSubjectLength) ||
			actor.GitLab.UserID != actor.Subject {
			return Actor{}, unauthenticated("authenticated GitLab actor identity is invalid")
		}
		gitlab := *actor.GitLab
		actor.GitLab = &gitlab
	default:
		return Actor{}, unauthenticated("authenticated actor channel is invalid")
	}

	return actor, nil
}

func canonicalMutationActor(actor Actor) (Actor, Principal, error) {
	canonical, err := canonicalActor(actor)
	if err != nil {
		return Actor{}, Principal{}, err
	}
	principal, err := principalForActor(canonical)
	if err != nil {
		return Actor{}, Principal{}, err
	}

	return canonical, principal, nil
}

func principalForActor(actor Actor) (Principal, error) {
	var principal Principal
	switch actor.Channel {
	case REST:
		principal = Principal{Kind: "rest_user", Issuer: actor.Issuer, ID: actor.Subject}
	case GoogleChat:
		principal = Principal{Kind: "google_chat_user", Issuer: actor.Issuer, ID: actor.Subject}
	case GitLabCI:
		encoded, err := json.Marshal([2]string{actor.GitLab.ProjectID, actor.GitLab.JobID})
		if err != nil {
			return Principal{}, fmt.Errorf("marshal GitLab principal identity: %w", err)
		}
		digest := sha256.Sum256(encoded)
		principal = Principal{Kind: "gitlab_job", Issuer: actor.GitLab.Issuer, ID: hex.EncodeToString(digest[:])}
	default:
		return Principal{}, unauthenticated("authenticated actor channel is invalid")
	}
	if !validOpaqueASCII(principal.Issuer, maxIssuerLength) || !validOpaqueASCII(principal.ID, maxSubjectLength) {
		return Principal{}, unauthenticated("authenticated actor identity is invalid")
	}

	return principal, nil
}

func canonicalRequestID(value string) (string, error) {
	if !validOpaqueASCII(value, maxRequestIDLength) {
		return "", invalid("requestId must contain 1 to 128 visible ASCII characters")
	}

	return value, nil
}

func canonicalClaimID(value string) (string, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", invalid("claimId must be a UUID")
	}
	for i, character := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if character < '0' || (character > '9' && character < 'A') || (character > 'F' && character < 'a') || character > 'f' {
			return "", invalid("claimId must be a UUID")
		}
	}

	return strings.ToLower(value), nil
}

func validOpaqueASCII(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}

	return true
}

func hasRFC3339Offset(value string) bool {
	if strings.HasSuffix(value, "Z") {
		return true
	}
	if len(value) < 6 {
		return false
	}
	offset := value[len(value)-6:]

	return (offset[0] == '+' || offset[0] == '-') && offset[3] == ':'
}

func hasSpaceSeparatedOffset(value string) bool {
	if !strings.Contains(value, " ") {
		return false
	}

	return strings.HasSuffix(value, "Z") || hasRFC3339Offset(value)
}

func berlinWallTimeCandidates(wall time.Time, location *time.Location) []time.Time {
	const sampleRange = 48 * time.Hour
	const sampleStep = 12 * time.Hour
	offsets := make(map[int]struct{}, 2)
	for delta := -sampleRange; delta <= sampleRange; delta += sampleStep {
		_, offset := wall.Add(delta).In(location).Zone()
		offsets[offset] = struct{}{}
	}

	candidates := make([]time.Time, 0, 2)
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Year() != wall.Year() || local.Month() != wall.Month() || local.Day() != wall.Day() ||
			local.Hour() != wall.Hour() || local.Minute() != wall.Minute() || local.Second() != wall.Second() ||
			local.Nanosecond() != wall.Nanosecond() {
			continue
		}
		duplicate := false
		for _, existing := range candidates {
			if existing.Equal(candidate) {
				duplicate = true

				break
			}
		}
		if !duplicate {
			candidates = append(candidates, candidate.UTC())
		}
	}

	return candidates
}

func unauthenticated(message string) error {
	return &Error{Code: Unauthenticated, Message: message}
}
