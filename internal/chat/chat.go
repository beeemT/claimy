// Package chat handles Google Chat claim commands.
package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	// Keep the IANA database available in minimal runtime images.
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	"github.com/gosoline-project/httpserver"
)

const (
	// EventPath is the HTTP path for Google Chat events.
	EventPath                   = "/v1/chat/events"
	maxRequestBytes             = 1 << 20
	maxInteractionDeadline      = 30 * time.Second
	maxResponseBytes            = 3800
	chatServiceUnavailableReply = "The claim service could not confirm the result. Redeliver the same Chat message to safely check it."
)

// Settings configures the Google Chat event handler.
type Settings struct {
	AppIdentity   string        `cfg:"app_identity"`
	AllowedSpaces []string      `cfg:"allowed_spaces"`
	Deadline      time.Duration `cfg:"deadline"`
}

// Handler authenticates and handles Google Chat claim commands.
type Handler struct {
	service       claims.Operations
	identities    auth.Authenticator
	appIdentity   string
	allowedSpaces map[string]struct{}
	deadline      time.Duration
	berlin        *time.Location
}

type chatEvent struct {
	Type    string       `json:"type"`
	User    auth.User    `json:"user"`
	Space   eventSpace   `json:"space"`
	Message eventMessage `json:"message"`
}

type eventSpace struct {
	Name string `json:"name"`
}

type eventMessage struct {
	Name         string        `json:"name"`
	Text         string        `json:"text"`
	ArgumentText string        `json:"argumentText"`
	SlashCommand *slashCommand `json:"slashCommand"`
	Sender       *auth.User    `json:"sender"`
}

type slashCommand struct {
	CommandID int64 `json:"commandId"`
}

func (s *slashCommand) UnmarshalJSON(data []byte) error {
	var encoded struct {
		CommandID json.RawMessage `json:"commandId"`
	}
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}

	id, err := parseSlashCommandID(encoded.CommandID)
	if err != nil {
		return err
	}
	s.CommandID = id

	return nil
}

func parseSlashCommandID(encoded json.RawMessage) (int64, error) {
	if len(encoded) == 0 {
		return 0, errors.New("slash command id is missing")
	}
	if encoded[0] != '"' {
		var id int64
		if err := json.Unmarshal(encoded, &id); err != nil {
			return 0, errors.New("slash command id is invalid")
		}

		return id, nil
	}

	var value string
	if err := json.Unmarshal(encoded, &value); err != nil || value == "" {
		return 0, errors.New("slash command id is invalid")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, errors.New("slash command id is invalid")
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New("slash command id is invalid")
	}

	return id, nil
}

type chatResponse struct {
	Text string `json:"text"`
}

// New validates settings and creates a Chat event handler.
func New(service claims.Operations, identities auth.Authenticator, settings Settings) (*Handler, error) {
	if service == nil {
		return nil, fmt.Errorf("chat operations are required")
	}
	if identities == nil {
		return nil, fmt.Errorf("chat authenticator is required")
	}
	if settings.AppIdentity == "" || strings.TrimSpace(settings.AppIdentity) != settings.AppIdentity || len(settings.AppIdentity) > 512 {
		return nil, fmt.Errorf("a valid chat app identity is required")
	}
	for _, r := range settings.AppIdentity {
		if unicode.IsControl(r) {
			return nil, fmt.Errorf("a valid chat app identity is required")
		}
	}
	if settings.Deadline <= 0 || settings.Deadline > maxInteractionDeadline {
		return nil, fmt.Errorf("chat interaction deadline must be between 1ns and %s", maxInteractionDeadline)
	}
	if len(settings.AllowedSpaces) == 0 {
		return nil, fmt.Errorf("at least one chat space must be allowed")
	}

	allowed := make(map[string]struct{}, len(settings.AllowedSpaces))
	for _, name := range settings.AllowedSpaces {
		if !validSpaceName(name) {
			return nil, fmt.Errorf("invalid configured chat space name")
		}
		if _, exists := allowed[name]; exists {
			return nil, fmt.Errorf("duplicate configured chat space name")
		}
		allowed[name] = struct{}{}
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return nil, fmt.Errorf("load Europe/Berlin time zone: %w", err)
	}

	return &Handler{
		service:       service,
		identities:    identities,
		appIdentity:   settings.AppIdentity,
		allowedSpaces: allowed,
		deadline:      settings.Deadline,
		berlin:        berlin,
	}, nil
}

// Register mounts the Chat event handler on the router.
func Register(router *httpserver.Router, handler *Handler) {
	router.POST(EventPath, httpserver.BindNR(handler.handle))
}

func (h *Handler) handle(_ context.Context, request *http.Request) (httpserver.Response, error) {
	ctx, cancel := newInteractionContext(request.Context(), h.deadline)
	defer cancel()

	event, err := decodeEvent(request)
	if err != nil {
		return h.respond(http.StatusBadRequest, "The Chat event could not be read."), nil
	}
	if event.Type != "MESSAGE" {
		return h.respond(http.StatusBadRequest, "Only Chat MESSAGE events are supported."), nil
	}

	token, ok := bearerToken(request.Header.Get(httpserver.HeaderAuthorization))
	if !ok {
		return h.respond(http.StatusUnauthorized, "The Google Chat request could not be verified."), nil
	}
	if event.User.Type != "HUMAN" || !validUserName(event.User.Name) {
		return h.respond(http.StatusUnauthorized, "A valid human Chat user is required."), nil
	}
	if event.Message.Sender != nil && !sameEventUser(event.User, *event.Message.Sender) {
		return h.respond(http.StatusUnauthorized, "The Chat message user does not match the event user."), nil
	}

	actor, err := h.identities.Chat(ctx, token, event.User)
	if err != nil {
		status, text := safeError(err)

		return h.respond(status, text), nil
	}
	if actor.Channel != claims.GoogleChat || actor.Subject != event.User.Name {
		return h.respond(http.StatusUnauthorized, "The Google Chat request could not be verified."), nil
	}
	if !validSpaceName(event.Space.Name) {
		return h.respond(http.StatusBadRequest, "The Chat event has an invalid space name."), nil
	}
	if _, allowed := h.allowedSpaces[event.Space.Name]; !allowed {
		return h.respond(http.StatusForbidden, "This Chat space is not enabled for claims."), nil
	}
	if event.Message.SlashCommand == nil || event.Message.SlashCommand.CommandID <= 0 {
		return h.respond(http.StatusBadRequest, "Use the configured /claim slash command."), nil
	}

	parsed, err := parseCommand(event.Message.Text, event.Message.ArgumentText)
	if err != nil {
		return h.respond(http.StatusOK, commandUsage), nil
	}
	requestID, err := h.requestID(parsed, event.Space.Name, event.Message.Name)
	if err != nil {
		return h.respond(http.StatusBadRequest, "The Chat message is missing its stable message identity."), nil
	}

	text, status := h.execute(ctx, actor, parsed, requestID)

	return h.respond(status, text), nil
}

const commandUsage = "Use /claim take <sandbox|prod|both> <group> [app <app>] [until <Berlin time>], " +
	"/claim free <sandbox|prod|both> <group> [app <app>] [at <time>], " +
	"/claim list [sandbox|prod|both] [at <time>], /claim release <claim-id>, " +
	"or /claim expiry <claim-id> until <Berlin time>."

func decodeEvent(request *http.Request) (chatEvent, error) {
	if request.Body == nil {
		return chatEvent{}, errors.New("empty Chat event body")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil {
		return chatEvent{}, err
	}
	if len(body) == 0 || len(body) > maxRequestBytes {
		return chatEvent{}, errors.New("invalid Chat event size")
	}
	var event chatEvent
	if err = json.Unmarshal(body, &event); err != nil {
		return chatEvent{}, err
	}

	return event, nil
}

func (h *Handler) requestID(parsed command, spaceName string, messageName string) (string, error) {
	if parsed.kind == commandFree || parsed.kind == commandList {
		return "", nil
	}
	if !validMessageName(messageName, spaceName) {
		return "", fmt.Errorf("invalid Chat message resource name")
	}
	key, err := json.Marshal([3]string{h.appIdentity, spaceName, messageName})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(key)

	return hex.EncodeToString(digest[:]), nil
}

func (h *Handler) execute(ctx context.Context, actor claims.Actor, parsed command, requestID string) (string, int) {
	switch parsed.kind {
	case commandTake:
		return h.executeTake(ctx, actor, parsed, requestID)
	case commandFree, commandList:
		return h.executeQuery(ctx, actor, parsed)
	case commandRelease:
		return h.executeRelease(ctx, actor, parsed, requestID)
	case commandExpiry:
		return h.executeExpiry(ctx, actor, parsed, requestID)
	default:
		return commandUsage, http.StatusBadRequest
	}
}

func (h *Handler) executeTake(ctx context.Context, actor claims.Actor, parsed command, requestID string) (string, int) {
	result, err := h.service.Acquire(ctx, actor, claims.AcquireRequest{
		Scope:        parsed.scope,
		Environments: parsed.environments,
		ExpiresAt:    parsed.expiresAt,
		RequestID:    requestID,
	})
	if err != nil {
		return safeOperationReply(err)
	}
	if !result.Acquired {
		return h.formatBusy(parsed, result.Conflicts), http.StatusOK
	}
	if result.Claim == nil {
		return safeOperationReply(nil)
	}

	return h.formatAcquired(*result.Claim), http.StatusOK
}

func (h *Handler) executeQuery(ctx context.Context, actor claims.Actor, parsed command) (string, int) {
	result, err := h.service.Query(ctx, actor, claims.QueryRequest{
		Scope:        parsed.scope,
		Environments: parsed.environments,
		At:           parsed.at,
	})
	if err != nil {
		return safeOperationReply(err)
	}
	switch parsed.kind {
	case commandFree:
		return h.formatFree(parsed, result), http.StatusOK
	case commandList:
		return h.formatList(result), http.StatusOK
	default:
		return commandUsage, http.StatusBadRequest
	}
}

func (h *Handler) executeRelease(ctx context.Context, actor claims.Actor, parsed command, requestID string) (string, int) {
	result, err := h.service.Release(ctx, actor, claims.ReleaseRequest{
		ClaimID:   parsed.claimID,
		RequestID: requestID,
	})
	if err != nil {
		return safeOperationReply(err)
	}
	if result.Claim.ID == "" {
		return safeOperationReply(nil)
	}
	if result.Changed {
		return fmt.Sprintf("Released claim %s for %s.", code(result.Claim.ID), formatScope(result.Claim.Scope)), http.StatusOK
	}

	return fmt.Sprintf("Claim %s is already inactive; no release was needed.", code(result.Claim.ID)), http.StatusOK
}

func (h *Handler) executeExpiry(ctx context.Context, actor claims.Actor, parsed command, requestID string) (string, int) {
	result, err := h.service.ChangeExpiry(ctx, actor, claims.ExpiryRequest{
		ClaimID:          parsed.claimID,
		ExpiresAt:        *parsed.expiresAt,
		ExpectedRevision: nil,
		RequestID:        requestID,
	})
	if err != nil {
		return safeOperationReply(err)
	}
	if result.Claim.ID == "" {
		return safeOperationReply(nil)
	}
	if result.Changed {
		return fmt.Sprintf(
			"Expiry updated for claim %s to %s (revision %d).",
			code(result.Claim.ID), h.formatTime(result.Claim.ExpiresAt), result.Claim.Revision,
		), http.StatusOK
	}

	return fmt.Sprintf(
		"Claim %s already has expiry %s; no change was made.",
		code(result.Claim.ID), h.formatTime(result.Claim.ExpiresAt),
	), http.StatusOK
}

func (h *Handler) respond(status int, text string) httpserver.Response {
	return httpserver.NewJsonResponse(chatResponse{Text: boundResponse(text)}, httpserver.WithStatusCode(status))
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}

	return parts[1], true
}

func validSpaceName(name string) bool {
	return validResourceName(name, "spaces/")
}

func validUserName(name string) bool {
	return validResourceName(name, "users/")
}

func validResourceName(name string, prefix string) bool {
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix) {
		return false
	}
	for _, character := range name[len(prefix):] {
		if !isResourceNameCharacter(character) {
			return false
		}
	}

	return true
}

func isResourceNameCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '_' ||
		character == '-'
}

func sameEventUser(eventUser auth.User, sender auth.User) bool {
	if sender.Name != eventUser.Name || sender.Type != "" && sender.Type != "HUMAN" {
		return false
	}
	if sender.Email != "" && !strings.EqualFold(strings.TrimSpace(sender.Email), strings.TrimSpace(eventUser.Email)) {
		return false
	}

	return true
}

func validMessageName(name string, spaceName string) bool {
	prefix := spaceName + "/messages/"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) || len(name) > 4096 {
		return false
	}
	for _, r := range name[len(prefix):] {
		if r < 0x21 || r > 0x7e || r == '/' {
			return false
		}
	}

	return true
}

func (h *Handler) formatAcquired(claim claims.Claim) string {
	if !claim.ActiveNow {
		return fmt.Sprintf("This message previously acquired claim %s, but it is no longer active. It must not be treated as a current claim.", code(claim.ID))
	}

	return fmt.Sprintf("Claim acquired: %s in %s for %s, expiring %s (claim %s).", formatEnvironments(claim.Environments), formatScope(claim.Scope), code(claim.OwnerEmail), h.formatTime(claim.ExpiresAt), code(claim.ID))
}

func (h *Handler) formatBusy(request command, conflicts []claims.Conflict) string {
	if len(conflicts) == 0 {
		return fmt.Sprintf(
			"Busy: %s in %s is not available.",
			formatScope(request.scope), formatEnvironments(request.environments),
		)
	}
	lines := []string{
		fmt.Sprintf(
			"Busy: %s in %s is not available.",
			formatScope(request.scope), formatEnvironments(request.environments),
		),
	}
	for _, conflict := range conflicts {
		environments := intersectEnvironments(request.environments, conflict.Environments)
		lines = append(lines, fmt.Sprintf(
			"Blocking claim %s: owner %s, scope %s, environment %s, expires %s.",
			code(conflict.ID), code(conflict.OwnerEmail), formatScope(conflict.Scope),
			formatEnvironments(environments), h.formatTime(conflict.ExpiresAt),
		))
	}

	return strings.Join(lines, "\n")
}

func (h *Handler) formatFree(request command, result claims.QueryResult) string {
	lines := []string{
		fmt.Sprintf("%s in %s", formatScope(request.scope), formatEnvironments(request.environments)),
		fmt.Sprintf("Free: %t", result.Free),
		fmt.Sprintf("Allowed for you: %t", result.AllowedForCaller),
	}
	if !result.Known {
		lines = append(lines, "This target is not registered.")
	}
	checkedAt := fmt.Sprintf("Checked at %s.", h.formatTime(result.At))
	if result.Projected {
		checkedAt += " This is a future projection, not a reservation."
	}
	lines = append(lines, checkedAt)
	for _, claim := range orderedClaims(result.Claims) {
		lines = append(lines, fmt.Sprintf(
			"Claim %s: owner %s, scope %s, environment %s, expires %s%s.",
			code(claim.ID), code(claim.OwnerEmail), formatScope(claim.Scope),
			formatEnvironments(claim.Environments), h.formatTime(claim.ExpiresAt),
			inheritedLabel(claim.Inherited),
		))
	}

	return strings.Join(lines, "\n")
}

func (h *Handler) formatList(result claims.QueryResult) string {
	heading := fmt.Sprintf("Claims at %s", h.formatTime(result.At))
	if result.Projected {
		heading += " (future projection, not a reservation)"
	}
	if len(result.Claims) == 0 {
		return heading + ": none."
	}
	lines := []string{heading + ":"}
	for _, claim := range orderedClaims(result.Claims) {
		lines = append(lines, fmt.Sprintf(
			"%s — owner %s; started %s; expires %s; source %s; environments %s%s.",
			formatScope(claim.Scope), code(claim.OwnerEmail),
			h.formatTime(claim.CreatedAt), h.formatTime(claim.ExpiresAt),
			formatSource(claim.Source), formatEnvironments(claim.Environments),
			inheritedLabel(claim.Inherited),
		))
	}

	return strings.Join(lines, "\n")
}

func formatSource(source claims.Source) string {
	switch source {
	case claims.Manual:
		return code("manual")
	case claims.CI:
		return code("ci")
	default:
		return code("unknown")
	}
}

func (h *Handler) formatTime(value time.Time) string {
	return value.In(h.berlin).Format("2006-01-02 15:04:05 MST")
}

func formatScope(scope claims.Scope) string {
	if scope.App == "" {
		return "group " + code(scope.Group)
	}

	return "group " + code(scope.Group) + ", app " + code(scope.App)
}

func formatEnvironments(environments []claims.Environment) string {
	if len(environments) == 0 {
		return "none"
	}
	sandboxCount, prodCount, unknownCount := 0, 0, 0
	for _, environment := range environments {
		switch environment {
		case claims.Sandbox:
			sandboxCount++
		case claims.Prod:
			prodCount++
		default:
			unknownCount++
		}
	}
	parts := make([]string, 0, len(environments))
	for range sandboxCount {
		parts = append(parts, formatEnvironment(claims.Sandbox))
	}
	for range prodCount {
		parts = append(parts, formatEnvironment(claims.Prod))
	}
	for range unknownCount {
		parts = append(parts, formatEnvironment(""))
	}

	return strings.Join(parts, ", ")
}

func formatEnvironment(environment claims.Environment) string {
	switch environment {
	case claims.Sandbox:
		return string(claims.Sandbox)
	case claims.Prod:
		return string(claims.Prod)
	default:
		return "unknown"
	}
}

func intersectEnvironments(requested []claims.Environment, existing []claims.Environment) []claims.Environment {
	if len(requested) == 0 {
		return append([]claims.Environment(nil), existing...)
	}
	set := make(map[claims.Environment]struct{}, len(requested))
	for _, environment := range requested {
		set[environment] = struct{}{}
	}
	intersection := make([]claims.Environment, 0, len(existing))
	for _, environment := range existing {
		if _, ok := set[environment]; ok {
			intersection = append(intersection, environment)
		}
	}

	return intersection
}

func inheritedLabel(inherited bool) string {
	if inherited {
		return "; inherited group coverage"
	}

	return ""
}

func orderedClaims(input []claims.Claim) []claims.Claim {
	ordered := append([]claims.Claim(nil), input...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		if ordered[i].Scope.Group != ordered[j].Scope.Group {
			return ordered[i].Scope.Group < ordered[j].Scope.Group
		}
		if ordered[i].Scope.App != ordered[j].Scope.App {
			return ordered[i].Scope.App < ordered[j].Scope.App
		}

		return ordered[i].ID < ordered[j].ID
	})

	return ordered
}

func code(value string) string {
	value = strings.ReplaceAll(value, "`", "ˋ")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, value)

	return "`" + value + "`"
}

func boundResponse(value string) string {
	if len(value) <= maxResponseBytes {
		return value
	}
	const suffix = "\nResults truncated to fit in a Chat reply."
	end := maxResponseBytes - len(suffix)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}

	return value[:end] + suffix
}

func safeError(err error) (int, string) {
	if err == nil {
		return http.StatusServiceUnavailable, chatServiceUnavailableReply
	}
	var domainError *claims.Error
	if !errors.As(err, &domainError) {
		return http.StatusUnauthorized, "The Google Chat request could not be verified."
	}
	switch domainError.Code {
	case claims.Unauthenticated:
		return http.StatusUnauthorized, "The Google Chat request could not be verified."
	case claims.Forbidden:
		return http.StatusForbidden, "Your account is not a member of the configured team."
	case claims.Invalid:
		return http.StatusBadRequest, "The command was rejected as invalid."
	case claims.NotFound:
		return http.StatusNotFound, "The requested claim was not found."
	case claims.HistoryUnavailable:
		return http.StatusGone, "That history is outside the available retention period."
	case claims.ConflictError:
		return http.StatusConflict, "This Chat message was already used for a different command. Send the changed command as a new message."
	case claims.StorageError:
		return http.StatusServiceUnavailable, chatServiceUnavailableReply
	default:
		return http.StatusServiceUnavailable, chatServiceUnavailableReply
	}
}

func safeOperationError(err error) (int, string) {
	if err != nil {
		var domainError *claims.Error
		if errors.As(err, &domainError) {
			return safeError(err)
		}
	}

	return http.StatusServiceUnavailable, chatServiceUnavailableReply
}

func safeOperationReply(err error) (string, int) {
	status, text := safeOperationError(err)

	return text, status
}

func newInteractionContext(parent context.Context, deadline time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, deadline)
}
