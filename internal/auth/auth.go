// Package auth verifies identity tokens and creates Claimy actors.
package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/beeemT/claimy/internal/claims"
)

const (
	googleIssuer = "https://accounts.google.com"
	maxTokenSize = 64 << 10
)

// Verifier establishes REST, GitLab CI, and Google Chat identities from signed JWTs.
type Verifier struct {
	teamDomain   string
	restIssuer   IssuerSettings
	gitLabIssuer IssuerSettings
	chatIssuer   IssuerSettings
	restIssuers  map[string]IssuerSettings
	keys         KeyProvider
	now          func() time.Time
}

var _ Authenticator = (*Verifier)(nil)

// New creates a verifier backed by the trusted, configured HTTPS JWKS endpoints.
// The configured URLs are the only key sources; token headers and claims cannot
// select a URL or otherwise influence where keys are fetched from.
func New(settings Settings) (*Verifier, error) {
	teamDomain, restIssuers, err := validateSettings(settings)
	if err != nil {
		return nil, err
	}
	provider, err := newHTTPKeyProvider(settings)
	if err != nil {
		return nil, err
	}

	return &Verifier{
		teamDomain:   teamDomain,
		restIssuer:   settings.REST,
		gitLabIssuer: settings.GitLab,
		chatIssuer:   settings.Chat,
		restIssuers:  restIssuers,
		keys:         provider,
		now:          time.Now,
	}, nil
}

// NewWithKeys creates the same real JWT verifier with a caller-supplied key
// provider. A provider reports an unknown kid as (nil, nil); its errors denote
// key-service failures and are returned as safe service errors by the verifier.
func NewWithKeys(settings Settings, provider KeyProvider) (*Verifier, error) {
	if provider == nil {
		return nil, errors.New("key provider is required")
	}
	teamDomain, restIssuers, err := validateSettings(settings)
	if err != nil {
		return nil, err
	}

	return &Verifier{
		teamDomain:   teamDomain,
		restIssuer:   settings.REST,
		gitLabIssuer: settings.GitLab,
		chatIssuer:   settings.Chat,
		restIssuers:  restIssuers,
		keys:         provider,
		now:          time.Now,
	}, nil
}

func validateSettings(settings Settings) (string, map[string]IssuerSettings, error) {
	teamDomain, err := canonicalTeamDomain(settings.TeamDomain)
	if err != nil {
		return "", nil, err
	}
	for _, configured := range []struct {
		name   string
		issuer IssuerSettings
	}{
		{name: "rest", issuer: settings.REST},
		{name: "gitlab", issuer: settings.GitLab},
		{name: "chat", issuer: settings.Chat},
	} {
		if err := validateIssuerSettings(configured.name, configured.issuer); err != nil {
			return "", nil, err
		}
	}
	if settings.Chat.Issuer != googleIssuer {
		return "", nil, errors.New("chat issuer must be the Google issuer")
	}
	if settings.REST.Issuer == settings.GitLab.Issuer {
		return "", nil, errors.New("REST and GitLab issuers must differ")
	}

	configuredJWKS := map[string]string{}
	for _, issuer := range []IssuerSettings{settings.REST, settings.GitLab, settings.Chat} {
		if endpoint, exists := configuredJWKS[issuer.Issuer]; exists && endpoint != issuer.JWKSURL {
			return "", nil, errors.New("one issuer cannot use multiple JWKS URLs")
		}
		configuredJWKS[issuer.Issuer] = issuer.JWKSURL
	}

	restIssuers := map[string]IssuerSettings{
		settings.REST.Issuer:   settings.REST,
		settings.GitLab.Issuer: settings.GitLab,
	}

	return teamDomain, restIssuers, nil
}

func validateIssuerSettings(label string, settings IssuerSettings) error {
	if err := validateHTTPSURL(settings.Issuer); err != nil {
		return fmt.Errorf("%s issuer must be an absolute HTTPS URL", label)
	}
	if err := validateHTTPSURL(settings.JWKSURL); err != nil {
		return fmt.Errorf("%s JWKS URL must be an absolute HTTPS URL", label)
	}
	if settings.Audience == "" || len(settings.Audience) > 1024 || strings.TrimSpace(settings.Audience) != settings.Audience {
		return fmt.Errorf("%s audience is required", label)
	}
	for _, r := range settings.Audience {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("%s audience contains invalid characters", label)
		}
	}

	return nil
}

func validateHTTPSURL(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return errors.New("URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("URL must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return errors.New("URL contains invalid characters")
		}
	}

	return nil
}

func canonicalTeamDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || len(domain) > 253 || strings.HasSuffix(domain, ".") {
		return "", errors.New("team domain is required")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("team domain is invalid")
		}
		for _, r := range label {
			if !isASCIILowerAlphaNumeric(r) && r != '-' {
				return "", errors.New("team domain is invalid")
			}
		}
	}

	return domain, nil
}

func isASCIILowerAlphaNumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// REST authenticates either the configured manual identity issuer or the
// configured GitLab CI issuer. The untrusted issuer claim is used only as an
// exact lookup key into that fixed allowlist, before signature verification.
func (v *Verifier) REST(ctx context.Context, token string) (claims.Actor, error) {
	parsed, err := parseJWT(token)
	if err != nil {
		return claims.Actor{}, unauthenticated()
	}
	issuer, ok := jsonString(parsed.claims, "iss")
	if !ok {
		return claims.Actor{}, unauthenticated()
	}
	settings, ok := v.restIssuers[issuer]
	if !ok {
		return claims.Actor{}, unauthenticated()
	}
	if err := v.verify(ctx, settings, parsed); err != nil {
		return claims.Actor{}, err
	}
	if issuer == v.gitLabIssuer.Issuer {
		return v.gitLabActor(parsed.claims)
	}

	return v.manualActor(parsed.claims)
}

// Chat first verifies the Google Chat service-account ID token, then binds the
// claim owner to the separately authenticated HUMAN user in the event.
func (v *Verifier) Chat(ctx context.Context, token string, user User) (claims.Actor, error) {
	parsed, err := parseJWT(token)
	if err != nil {
		return claims.Actor{}, unauthenticated()
	}
	if err := v.verify(ctx, v.chatIssuer, parsed); err != nil {
		return claims.Actor{}, err
	}
	email, emailOK := jsonString(parsed.claims, "email")
	verified, verifiedOK := jsonBool(parsed.claims, "email_verified")
	if !emailOK || !verifiedOK || !verified || email != "chat@system.gserviceaccount.com" {
		return claims.Actor{}, unauthenticated()
	}
	if user.Type != "HUMAN" {
		return claims.Actor{}, unauthenticated()
	}
	if !validResourceUserName(user.Name) {
		return claims.Actor{}, unauthenticated()
	}
	canonicalEmail, err := canonicalAccountEmail(user.Email)
	if err != nil {
		return claims.Actor{}, unauthenticated()
	}
	if !v.inTeam(canonicalEmail) {
		return claims.Actor{}, forbidden()
	}

	return claims.Actor{
		Email:   canonicalEmail,
		Issuer:  v.chatIssuer.Issuer,
		Subject: user.Name,
		Channel: claims.GoogleChat,
	}, nil
}

func (v *Verifier) manualActor(tokenClaims map[string]json.RawMessage) (claims.Actor, error) {
	subject, ok := jsonString(tokenClaims, "sub")
	if !ok || !validStableID(subject) {
		return claims.Actor{}, unauthenticated()
	}
	email, ok := jsonString(tokenClaims, "email")
	if !ok {
		return claims.Actor{}, unauthenticated()
	}
	canonicalEmail, err := canonicalAccountEmail(email)
	if err != nil {
		return claims.Actor{}, unauthenticated()
	}
	if !v.inTeam(canonicalEmail) {
		return claims.Actor{}, forbidden()
	}

	return claims.Actor{
		Email:   canonicalEmail,
		Issuer:  v.restIssuer.Issuer,
		Subject: subject,
		Channel: claims.REST,
	}, nil
}

func (v *Verifier) gitLabActor(tokenClaims map[string]json.RawMessage) (claims.Actor, error) {
	subject, subjectOK := jsonString(tokenClaims, "user_id")
	email, emailOK := jsonString(tokenClaims, "user_email")
	projectID, projectOK := gitLabProjectID(tokenClaims)
	jobID, jobOK := jsonString(tokenClaims, "job_id")
	if !subjectOK || !validStableID(subject) || !emailOK || !projectOK || !validStableID(projectID) || !jobOK || !validStableID(jobID) {
		return claims.Actor{}, unauthenticated()
	}
	canonicalEmail, err := canonicalAccountEmail(email)
	if err != nil {
		return claims.Actor{}, unauthenticated()
	}
	if !v.inTeam(canonicalEmail) {
		return claims.Actor{}, forbidden()
	}
	issuer, ok := jsonString(tokenClaims, "iss")
	if !ok || issuer != v.gitLabIssuer.Issuer {
		return claims.Actor{}, unauthenticated()
	}

	return claims.Actor{
		Email:   canonicalEmail,
		Issuer:  issuer,
		Subject: subject,
		Channel: claims.GitLabCI,
		GitLab: &claims.GitLabIdentity{
			Issuer:    issuer,
			ProjectID: projectID,
			JobID:     jobID,
			UserID:    subject,
		},
	}, nil
}

// GitLab's job_project_id identifies the project executing this job. For older
// tokens, project_id is accepted only when job_project_id is absent; a present
// but malformed job_project_id never falls back to the legacy claim.
func gitLabProjectID(tokenClaims map[string]json.RawMessage) (string, bool) {
	if raw, ok := tokenClaims["job_project_id"]; ok {
		var projectID string
		if json.Unmarshal(raw, &projectID) != nil || !validStableID(projectID) {
			return "", false
		}

		return projectID, true
	}
	projectID, ok := jsonString(tokenClaims, "project_id")

	return projectID, ok
}

func (v *Verifier) verify(ctx context.Context, settings IssuerSettings, parsed jwtToken) error {
	if ctx == nil || ctx.Err() != nil {
		return serviceUnavailable()
	}
	issuer, ok := jsonString(parsed.claims, "iss")
	if !ok || issuer != settings.Issuer || !exactlyOneAudience(parsed.claims, settings.Audience) || !validTimes(parsed.claims, v.now()) {
		return unauthenticated()
	}

	key, err := v.keys.Key(ctx, settings.Issuer, parsed.kid)
	if err != nil {
		return serviceUnavailable()
	}
	if key == nil {
		return unauthenticated()
	}
	publicKey, ok := key.(*rsa.PublicKey)
	if !ok || !validRSAPublicKey(publicKey) {
		return serviceUnavailable()
	}
	digest := sha256.Sum256([]byte(parsed.signingInput))
	if len(parsed.signature) != publicKey.Size() || rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], parsed.signature) != nil {
		return unauthenticated()
	}
	if !validTimes(parsed.claims, v.now()) {
		return unauthenticated()
	}
	if ctx.Err() != nil {
		return serviceUnavailable()
	}

	return nil
}

func validTimes(tokenClaims map[string]json.RawMessage, now time.Time) bool {
	expiry, ok := unixClaim(tokenClaims, "exp")
	if !ok || expiry <= now.Unix() {
		return false
	}
	if _, present := tokenClaims["nbf"]; present {
		notBefore, ok := unixClaim(tokenClaims, "nbf")
		if !ok || notBefore > now.Unix() {
			return false
		}
	}
	if _, present := tokenClaims["iat"]; present {
		issuedAt, ok := unixClaim(tokenClaims, "iat")
		if !ok || issuedAt > now.Unix() {
			return false
		}
	}

	return true
}

func unixClaim(tokenClaims map[string]json.RawMessage, name string) (int64, bool) {
	raw, ok := tokenClaims[name]
	if !ok {
		return 0, false
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || strings.ContainsAny(value, ".eE+") {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)

	return parsed, err == nil
}

func exactlyOneAudience(tokenClaims map[string]json.RawMessage, expected string) bool {
	raw, ok := tokenClaims["aud"]
	if !ok {
		return false
	}
	var audience string
	if json.Unmarshal(raw, &audience) == nil {
		return audience == expected
	}
	var audiences []string
	if json.Unmarshal(raw, &audiences) != nil || len(audiences) != 1 {
		return false
	}

	return audiences[0] == expected
}

func jsonString(object map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := object[name]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}

	return value, true
}

func jsonBool(object map[string]json.RawMessage, name string) (bool, bool) {
	raw, ok := object[name]
	if !ok {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}

	return value, true
}

func canonicalAccountEmail(email string) (string, error) {
	canonical := strings.ToLower(strings.TrimSpace(email))
	if canonical == "" || len(canonical) > 320 {
		return "", errors.New("email is invalid")
	}
	parsed, err := mail.ParseAddress(canonical)
	if err != nil || parsed.Address != canonical {
		return "", errors.New("email is invalid")
	}
	at := strings.LastIndexByte(canonical, '@')
	if at <= 0 || at == len(canonical)-1 {
		return "", errors.New("email is invalid")
	}

	return canonical, nil
}

func (v *Verifier) inTeam(email string) bool {
	return strings.EqualFold(email[strings.LastIndexByte(email, '@')+1:], v.teamDomain)
}

func validStableID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	first := value[0]
	if !isASCIIAlphaNumeric(rune(first)) {
		return false
	}
	for _, r := range value {
		if !isASCIIAlphaNumeric(r) && !strings.ContainsRune("._:-", r) {
			return false
		}
	}

	return true
}

func isASCIIAlphaNumeric(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func validResourceUserName(name string) bool {
	const prefix = "users/"
	if !strings.HasPrefix(name, prefix) || len(name) > 128 {
		return false
	}

	return validStableID(strings.TrimPrefix(name, prefix))
}

func validRSAPublicKey(key *rsa.PublicKey) bool {
	return key != nil && key.N != nil && key.N.Sign() > 0 && key.N.Bit(0) == 1 && key.E >= 3 && key.E <= 1<<31-1 && key.E&1 == 1 && key.N.BitLen() >= 2048 && key.N.BitLen() <= 8192
}

type jwtToken struct {
	kid          string
	signingInput string
	signature    []byte
	claims       map[string]json.RawMessage
}

func parseJWT(compact string) (jwtToken, error) {
	if compact == "" || len(compact) > maxTokenSize {
		return jwtToken{}, errors.New("invalid token")
	}
	segments := strings.Split(compact, ".")
	if len(segments) != 3 {
		return jwtToken{}, errors.New("invalid token")
	}
	headerBytes, err := base64.RawURLEncoding.Strict().DecodeString(segments[0])
	if err != nil {
		return jwtToken{}, errors.New("invalid token")
	}
	claimsBytes, err := base64.RawURLEncoding.Strict().DecodeString(segments[1])
	if err != nil {
		return jwtToken{}, errors.New("invalid token")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(segments[2])
	if err != nil {
		return jwtToken{}, errors.New("invalid token")
	}
	header, err := decodeJSONObject(headerBytes)
	if err != nil {
		return jwtToken{}, errors.New("invalid token")
	}
	algorithm, ok := jsonString(header, "alg")
	if !ok || algorithm != "RS256" {
		return jwtToken{}, errors.New("invalid token")
	}
	kid, ok := jsonString(header, "kid")
	if !ok || !validStableID(kid) {
		return jwtToken{}, errors.New("invalid token")
	}
	for _, unsupported := range []string{"jku", "x5u", "crit", "b64"} {
		if _, present := header[unsupported]; present {
			return jwtToken{}, errors.New("invalid token")
		}
	}
	payload, err := decodeJSONObject(claimsBytes)
	if err != nil {
		return jwtToken{}, errors.New("invalid token")
	}

	return jwtToken{
		kid:          kid,
		signingInput: segments[0] + "." + segments[1],
		signature:    signature,
		claims:       payload,
	}, nil
}

func decodeJSONObject(encoded []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, errors.New("invalid object")
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if _, duplicate := object[name]; duplicate {
			return nil, errors.New("duplicate object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid object value")
		}
		object[name] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, errors.New("invalid object end")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing object data")
	}

	return object, nil
}

func unauthenticated() error {
	return claims.NewError(claims.Unauthenticated, "authentication failed")
}

func forbidden() error {
	return claims.NewError(claims.Forbidden, "team membership required")
}

func serviceUnavailable() error {
	return &claims.Error{Code: claims.StorageError, Message: "identity provider unavailable"}
}
