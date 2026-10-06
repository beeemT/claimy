//go:build integration && fixtures

package chat

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	claimmysql "github.com/beeemT/claimy/internal/storage/mysql"
	"github.com/beeemT/claimy/test/support"
	"github.com/gin-gonic/gin"
	"github.com/gosoline-project/httpserver"
	"github.com/gosoline-project/sqlc"
)

const (
	chatTestIssuer   = "https://accounts.google.com"
	chatTestAudience = "https://claimy.example.com/chat"
	chatTestKeyID    = "claimy-chat-integration"
	chatTestSpace    = "spaces/AAAA-test-space"
)

type signedTestKeys struct {
	issuer string
	keyID  string
	key    *rsa.PublicKey
}

func (k signedTestKeys) Key(_ context.Context, issuer string, keyID string) (any, error) {
	if issuer != k.issuer || keyID != k.keyID {
		return nil, nil
	}

	return k.key, nil
}

type eventHuman struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

type eventSlashCommand struct {
	CommandID string `json:"commandId"`
}

type eventMessageFixture struct {
	Name         string             `json:"name"`
	Text         string             `json:"text"`
	ArgumentText string             `json:"argumentText"`
	SlashCommand *eventSlashCommand `json:"slashCommand"`
	Sender       *eventHuman        `json:"sender"`
}

type eventEnvelope struct {
	Type    string              `json:"type"`
	User    eventHuman          `json:"user"`
	Space   eventSpace          `json:"space"`
	Message eventMessageFixture `json:"message"`
}

func TestChatHTTPRouteAgainstMySQL(t *testing.T) {
	fixture := newChatHTTPFixture(t)

	t.Run("S15 team member manages manual and CI claims", func(t *testing.T) {
		testChatManualAndCIClaims(t, fixture)
	})
	t.Run("S26 different human identity conflicts and typed owner text is rejected", func(t *testing.T) {
		testChatIdentityAndOwnerText(t, fixture)
	})
	t.Run("S27 invalid event identities and spaces do not write", func(t *testing.T) {
		testChatInvalidIdentitiesAndSpaces(t, fixture)
	})
	t.Run("S28 same message replays once and changed intent is 409", func(t *testing.T) {
		testChatMessageIdempotency(t, fixture)
	})
	t.Run("S29 successful and busy bot replies", func(t *testing.T) {
		testChatBotReplies(t, fixture)
	})
}

func newSignedChatVerifier(t *testing.T) (*auth.Verifier, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	settings := auth.Settings{
		TeamDomain: "example.com",
		REST: auth.IssuerSettings{
			Issuer:   "https://manual.example.com",
			Audience: "https://claimy.example.com/rest",
			JWKSURL:  "https://keys.example.com/rest.json",
		},
		GitLab: auth.IssuerSettings{
			Issuer:   "https://gitlab.example.com",
			Audience: "https://claimy.example.com/gitlab",
			JWKSURL:  "https://keys.example.com/gitlab.json",
		},
		Chat: auth.IssuerSettings{
			Issuer:   chatTestIssuer,
			Audience: chatTestAudience,
			JWKSURL:  "https://keys.example.com/chat.json",
		},
	}
	verifier, err := auth.NewWithKeys(settings, signedTestKeys{issuer: chatTestIssuer, keyID: chatTestKeyID, key: &privateKey.PublicKey})
	if err != nil {
		t.Fatal(err)
	}

	return verifier, signedChatToken(t, privateKey)
}

func signedChatToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	now := time.Now().Unix()
	marshal := func(value any) []byte {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}

		return encoded
	}
	encode := func(value []byte) string {
		return base64.RawURLEncoding.EncodeToString(value)
	}
	header := encode(marshal(map[string]string{"alg": "RS256", "kid": chatTestKeyID, "typ": "JWT"}))
	claimsJSON := encode(marshal(map[string]any{
		"iss":            chatTestIssuer,
		"aud":            chatTestAudience,
		"sub":            "chat-service-account",
		"iat":            now,
		"nbf":            now - 1,
		"exp":            now + 3600,
		"email":          "chat@system.gserviceaccount.com",
		"email_verified": true,
	}))
	message := header + "." + claimsJSON
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func postChatEvent(t *testing.T, router http.Handler, token string, user auth.User, messageName string, command string, spaceName string) (*httptest.ResponseRecorder, chatResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, EventPath, bytes.NewReader(chatEventBytes(t, user, messageName, command, spaceName)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var body chatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode bot response %q: %v", response.Body.String(), err)
	}

	return response, body
}

func chatEventBytes(t *testing.T, user auth.User, messageName string, command string, spaceName string) []byte {
	t.Helper()
	human := eventHuman{Name: user.Name, Email: user.Email, Type: user.Type}
	event := eventEnvelope{
		Type:  "MESSAGE",
		User:  human,
		Space: eventSpace{Name: spaceName},
		Message: eventMessageFixture{
			Name:         messageName,
			Text:         "/claim " + command,
			ArgumentText: command,
			SlashCommand: &eventSlashCommand{CommandID: "42"},
			Sender:       &human,
		},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}

func seedClaim(t *testing.T, service claims.Operations, actor claims.Actor, requestID string, group string, app string) claims.Claim {
	t.Helper()
	expiresAt := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	result, err := service.Acquire(t.Context(), actor, claims.AcquireRequest{
		Scope:        claims.Scope{Group: group, App: app},
		Environments: []claims.Environment{claims.Sandbox},
		ExpiresAt:    &expiresAt,
		RequestID:    requestID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acquired || result.Claim == nil {
		t.Fatalf("seed acquisition did not succeed: %#v", result)
	}

	return *result.Claim
}

func queryClaim(t *testing.T, service claims.Operations, actor claims.Actor, scope claims.Scope) claims.QueryResult {
	t.Helper()
	result, err := service.Query(t.Context(), actor, claims.QueryRequest{
		Scope:        scope,
		Environments: []claims.Environment{claims.Sandbox},
	})
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func memberActor(user auth.User) claims.Actor {
	return claims.Actor{Email: user.Email, Issuer: chatTestIssuer, Subject: user.Name, Channel: claims.GoogleChat}
}

type chatHTTPFixture struct {
	client      sqlc.Client
	service     claims.Operations
	router      http.Handler
	token       string
	member      auth.User
	otherMember auth.User
	owner       auth.User
	manualActor claims.Actor
	ciActor     claims.Actor
}

func newChatHTTPFixture(t *testing.T) chatHTTPFixture {
	t.Helper()

	client := support.NewDatabase(t)
	service := claims.NewService(claimmysql.New(client))
	verifier, token := newSignedChatVerifier(t)
	handler, err := New(service, verifier, Settings{
		AppIdentity:   "projects/test-project/agents/claimy",
		AllowedSpaces: []string{chatTestSpace},
		Deadline:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Exercise the concrete handler binding used by Register with the real HTTP adapter.
	router.POST(EventPath, httpserver.BindNR(handler.handle))

	return chatHTTPFixture{
		client:      client,
		service:     service,
		router:      router,
		token:       token,
		member:      auth.User{Name: "users/team-member", Email: "member@example.com", Type: "HUMAN"},
		otherMember: auth.User{Name: "users/other-member", Email: "other@example.com", Type: "HUMAN"},
		owner:       auth.User{Name: "users/owner", Email: "alice@example.com", Type: "HUMAN"},
		manualActor: claims.Actor{
			Email:   "alice@example.com",
			Issuer:  "https://manual.example.com",
			Subject: "owner",
			Channel: claims.REST,
		},
		ciActor: claims.Actor{
			Email:   "ci-owner@example.com",
			Issuer:  "https://gitlab.example.com",
			Subject: "gitlab-user-77",
			Channel: claims.GitLabCI,
			GitLab: &claims.GitLabIdentity{
				Issuer:    "https://gitlab.example.com",
				ProjectID: "project-88",
				JobID:     "job-99",
				UserID:    "gitlab-user-77",
			},
		},
	}
}

func testChatManualAndCIClaims(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	manual := seedClaim(t, fixture.service, fixture.manualActor, "s15-manual", "manual-team", "api")
	ci := seedClaim(t, fixture.service, fixture.ciActor, "s15-ci", "ci-team", "worker")

	for _, test := range []struct {
		name   string
		claim  claims.Claim
		user   auth.User
		first  string
		second string
	}{
		{name: "manual claim", claim: manual, user: fixture.member, first: "s15-manual-expiry", second: "s15-manual-release"},
		{name: "CI claim", claim: ci, user: fixture.otherMember, first: "s15-ci-expiry", second: "s15-ci-release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testChatClaimLifecycle(t, fixture, test.claim, test.user, test.first, test.second)
		})
	}
}

func testChatClaimLifecycle(t *testing.T, fixture chatHTTPFixture, claim claims.Claim, user auth.User, expiryMessage string, releaseMessage string) {
	t.Helper()

	newExpiry := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	expiryCommand := fmt.Sprintf("expiry %s until %s", claim.ID, newExpiry.Format(time.RFC3339))
	response, body := postChatEvent(
		t, fixture.router, fixture.token, user,
		"spaces/AAAA-test-space/messages/"+expiryMessage, expiryCommand, chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Expiry updated") || !strings.Contains(body.Text, claim.ID) {
		t.Fatalf("expiry response = %d %q", response.Code, body.Text)
	}
	var audit struct {
		ActorEmail   string         `db:"actor_email"`
		ActorSubject string         `db:"actor_subject"`
		Channel      claims.Channel `db:"channel"`
	}
	err := fixture.client.Get(t.Context(), &audit, "SELECT actor_email, actor_subject, channel FROM claim_versions WHERE claim_id = ? ORDER BY revision DESC LIMIT 1", claim.ID)
	if err != nil {
		t.Fatalf("read committed Chat audit row: %v", err)
	}
	if audit.ActorEmail != user.Email || audit.ActorSubject != user.Name || audit.Channel != claims.GoogleChat {
		t.Fatalf("Chat audit actor = %#v, want authenticated event user", audit)
	}
	response, body = postChatEvent(
		t, fixture.router, fixture.token, user,
		"spaces/AAAA-test-space/messages/"+releaseMessage, "release "+claim.ID, chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Released claim") || !strings.Contains(body.Text, claim.ID) {
		t.Fatalf("release response = %d %q", response.Code, body.Text)
	}

	result := queryClaim(t, fixture.service, memberActor(user), claim.Scope)
	if len(result.Claims) != 0 {
		t.Fatalf("released claim remains visible as active: %#v", result.Claims)
	}
}

func testChatIdentityAndOwnerText(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	claim := seedClaim(t, fixture.service, fixture.manualActor, "s26-busy", "identity-mismatch", "service")
	response, body := postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s26-busy",
		"take sandbox identity-mismatch app service", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Busy") || !strings.Contains(body.Text, claim.OwnerEmail) || !strings.Contains(body.Text, claim.ID) || !strings.Contains(body.Text, "sandbox") || !strings.Contains(body.Text, "identity-mismatch") {
		t.Fatalf("busy response = %d %q", response.Code, body.Text)
	}

	response, body = postChatEvent(
		t, fixture.router, fixture.token, fixture.owner,
		"spaces/AAAA-test-space/messages/s26-free",
		"free sandbox identity-mismatch app service", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Free: false") || !strings.Contains(body.Text, "Allowed for you: true") {
		t.Fatalf("same-owner free response = %d %q", response.Code, body.Text)
	}

	response, body = postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s26-forged",
		"take sandbox identity-mismatch app service alice@example.com", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "/claim take") {
		t.Fatalf("typed owner command response = %d %q", response.Code, body.Text)
	}
}

func testChatInvalidIdentitiesAndSpaces(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	untrusted := chatEventBytes(
		t, fixture.member, "spaces/AAAA-test-space/messages/s27-invalid",
		"take sandbox denied-target", chatTestSpace,
	)
	request := httptest.NewRequest(http.MethodPost, EventPath, bytes.NewReader(untrusted))
	request.Header.Set("Authorization", "Bearer not-a-signed-token")
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid-token status = %d, want 401", response.Code)
	}

	response, _ = postChatEvent(
		t, fixture.router, fixture.token,
		auth.User{Name: "users/outside", Email: "outside@not-example.org", Type: "HUMAN"},
		"spaces/AAAA-test-space/messages/s27-nonteam",
		"take sandbox denied-target", chatTestSpace,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-team status = %d, want 403", response.Code)
	}
	response, _ = postChatEvent(
		t, fixture.router, fixture.token,
		auth.User{Name: "users/no-email", Type: "HUMAN"},
		"spaces/AAAA-test-space/messages/s27-no-email",
		"take sandbox denied-target", chatTestSpace,
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing-email status = %d, want 401", response.Code)
	}
	response, _ = postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/BBBB-other/messages/s27-space",
		"take sandbox denied-target", "spaces/BBBB-other",
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("disallowed-space status = %d, want 403", response.Code)
	}

	response, body := postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s27-read-only",
		"free sandbox denied-target", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Free: true") || !strings.Contains(body.Text, "This target is not registered") {
		t.Fatalf("unknown-target free response = %d %q", response.Code, body.Text)
	}
	unknown := queryClaim(t, fixture.service, memberActor(fixture.member), claims.Scope{Group: "denied-target"})
	if unknown.Known || len(unknown.Claims) != 0 {
		t.Fatalf("rejected requests registered a group or claim: %#v", unknown)
	}
}

func testChatMessageIdempotency(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	testChatTakeReplay(t, fixture)
	testChatExpiryReplay(t, fixture)
}

func testChatTakeReplay(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	messageName := "spaces/AAAA-test-space/messages/s28-take"
	first, firstBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, "take sandbox replay-group app worker", chatTestSpace)
	if first.Code != http.StatusOK || !strings.Contains(firstBody.Text, "Claim acquired") {
		t.Fatalf("initial take = %d %q", first.Code, firstBody.Text)
	}
	second, secondBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, "take sandbox replay-group app worker", chatTestSpace)
	if second.Code != http.StatusOK || secondBody.Text != firstBody.Text {
		t.Fatalf("duplicate take = %d %q, original = %q", second.Code, secondBody.Text, firstBody.Text)
	}
	changed, changedBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, "take prod different-group app worker", chatTestSpace)
	if changed.Code != http.StatusConflict || !strings.Contains(changedBody.Text, "different command") {
		t.Fatalf("changed take replay = %d %q, want 409 mismatch", changed.Code, changedBody.Text)
	}
	active := queryClaim(t, fixture.service, memberActor(fixture.member), claims.Scope{Group: "replay-group", App: "worker"})
	if len(active.Claims) != 1 || active.Claims[0].Revision != 1 {
		t.Fatalf("duplicate acquisition changed persisted state: %#v", active.Claims)
	}
}

func testChatExpiryReplay(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	seed := seedClaim(t, fixture.service, fixture.manualActor, "s28-expiry-seed", "expiry-replay", "scheduler")
	messageName := "spaces/AAAA-test-space/messages/s28-expiry"
	newExpiry := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	command := fmt.Sprintf("expiry %s until %s", seed.ID, newExpiry.Format(time.RFC3339))
	first, firstBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, command, chatTestSpace)
	if first.Code != http.StatusOK || !strings.Contains(firstBody.Text, "Expiry updated") {
		t.Fatalf("initial expiry = %d %q", first.Code, firstBody.Text)
	}
	second, secondBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, command, chatTestSpace)
	if second.Code != http.StatusOK || secondBody.Text != firstBody.Text {
		t.Fatalf("duplicate expiry = %d %q, original = %q", second.Code, secondBody.Text, firstBody.Text)
	}
	changedCommand := fmt.Sprintf("expiry %s until %s", seed.ID, newExpiry.Add(time.Hour).Format(time.RFC3339))
	changed, changedBody := postChatEvent(t, fixture.router, fixture.token, fixture.member, messageName, changedCommand, chatTestSpace)
	if changed.Code != http.StatusConflict || !strings.Contains(changedBody.Text, "different command") {
		t.Fatalf("changed expiry replay = %d %q, want 409 mismatch", changed.Code, changedBody.Text)
	}
	stored := queryClaim(t, fixture.service, memberActor(fixture.member), seed.Scope)
	if len(stored.Claims) != 1 || stored.Claims[0].Revision != 2 || !stored.Claims[0].ExpiresAt.Equal(newExpiry) {
		t.Fatalf("expiry redelivery changed history more than once: %#v", stored.Claims)
	}
}

func testChatBotReplies(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	testChatSuccessfulBusyAndListReplies(t, fixture)
	testChatProjectionReply(t, fixture)
}

func testChatSuccessfulBusyAndListReplies(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	response, body := postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s29-success",
		"take both sandbox-ready app api", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Claim acquired") || !strings.Contains(body.Text, "sandbox, prod") || !strings.Contains(body.Text, "expiring") {
		t.Fatalf("success reply = %d %q", response.Code, body.Text)
	}
	response, body = postChatEvent(
		t, fixture.router, fixture.token, fixture.otherMember,
		"spaces/AAAA-test-space/messages/s29-busy",
		"take prod sandbox-ready app api", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Busy") || !strings.Contains(body.Text, "member@example.com") || !strings.Contains(body.Text, "prod") {
		t.Fatalf("busy reply = %d %q", response.Code, body.Text)
	}
	response, body = postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s29-list", "list", chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Claims at ") || !strings.Contains(body.Text, "sandbox-ready") || !strings.Contains(body.Text, "started ") || !strings.Contains(body.Text, "source `manual`") {
		t.Fatalf("all-groups list response = %d %q", response.Code, body.Text)
	}
}

func testChatProjectionReply(t *testing.T, fixture chatHTTPFixture) {
	t.Helper()

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	response, body := postChatEvent(
		t, fixture.router, fixture.token, fixture.member,
		"spaces/AAAA-test-space/messages/s29-projection",
		"free sandbox sandbox-ready app api at "+future, chatTestSpace,
	)
	if response.Code != http.StatusOK || !strings.Contains(body.Text, "Free: true") || !strings.Contains(body.Text, "future projection, not a reservation") {
		t.Fatalf("future projection response = %d %q", response.Code, body.Text)
	}
}
