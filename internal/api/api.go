// Package api provides authenticated HTTP handlers for claim operations.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/pkg/client"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gosoline-project/httpserver"
	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

type service struct {
	ops        claims.Operations
	identities auth.Authenticator
	login      auth.Settings
}

func wireScope(in claims.Scope) client.Scope {
	out := client.Scope{Group: in.Group}
	if in.App != "" {
		app := in.App
		out.App = &app
	}

	return out
}

func wireGitLabIdentity(in *claims.GitLabIdentity) *client.GitLabIdentity {
	if in == nil {
		return nil
	}

	return &client.GitLabIdentity{
		Issuer:    in.Issuer,
		ProjectId: in.ProjectID,
		JobId:     in.JobID,
		UserId:    in.UserID,
	}
}

func wireUUID(id string) (openapi_types.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return openapi_types.UUID{}, fmt.Errorf("invalid claim identifier: %w", err)
	}

	return parsed, nil
}

func wireClaim(in claims.Claim) (client.Claim, error) {
	id, err := wireUUID(in.ID)
	if err != nil {
		return client.Claim{}, err
	}
	var environments []client.Environment
	if in.Environments != nil {
		environments = make([]client.Environment, len(in.Environments))
		for i, environment := range in.Environments {
			environments[i] = client.Environment(environment)
		}
	}

	return client.Claim{
		ActiveNow:    in.ActiveNow,
		CreatedAt:    in.CreatedAt,
		Environments: environments,
		ExpiresAt:    in.ExpiresAt,
		Gitlab:       wireGitLabIdentity(in.GitLab),
		Id:           id,
		Inherited:    in.Inherited,
		OwnerEmail:   openapi_types.Email(in.OwnerEmail),
		ReleasedAt:   in.ReleasedAt,
		Revision:     int64(in.Revision),
		Scope:        wireScope(in.Scope),
		Source:       client.Source(in.Source),
	}, nil
}

func wireConflict(in claims.Conflict) (client.Conflict, error) {
	id, err := wireUUID(in.ID)
	if err != nil {
		return client.Conflict{}, err
	}
	var environments []client.Environment
	if in.Environments != nil {
		environments = make([]client.Environment, len(in.Environments))
		for i, environment := range in.Environments {
			environments[i] = client.Environment(environment)
		}
	}

	return client.Conflict{
		Environments: environments,
		ExpiresAt:    in.ExpiresAt,
		Id:           id,
		OwnerEmail:   openapi_types.Email(in.OwnerEmail),
		Scope:        wireScope(in.Scope),
		Source:       client.Source(in.Source),
	}, nil
}

func wireAcquireResult(in claims.AcquireResult) (client.AcquireResponse, error) {
	out := client.AcquireResponse{Acquired: in.Acquired}
	if in.Claim != nil {
		claim, err := wireClaim(*in.Claim)
		if err != nil {
			return client.AcquireResponse{}, err
		}
		out.Claim = &claim
	}
	if len(in.Conflicts) > 0 {
		conflicts := make([]client.Conflict, len(in.Conflicts))
		for i, conflict := range in.Conflicts {
			converted, err := wireConflict(conflict)
			if err != nil {
				return client.AcquireResponse{}, err
			}
			conflicts[i] = converted
		}
		out.Conflicts = &conflicts
	}

	return out, nil
}

func wireQueryResult(in claims.QueryResult) (client.QueryResponse, error) {
	out := client.QueryResponse{
		AllowedForCaller: in.AllowedForCaller,
		At:               in.At,
		Free:             in.Free,
		Known:            in.Known,
		Projected:        in.Projected,
	}
	if in.Claims != nil {
		out.Claims = make([]client.Claim, len(in.Claims))
		for i, claim := range in.Claims {
			converted, err := wireClaim(claim)
			if err != nil {
				return client.QueryResponse{}, err
			}
			out.Claims[i] = converted
		}
	}

	return out, nil
}

func wireMutationResult(in claims.MutationResult) (client.MutationResponse, error) {
	claim, err := wireClaim(in.Claim)
	if err != nil {
		return client.MutationResponse{}, err
	}

	return client.MutationResponse{Changed: in.Changed, Claim: claim}, nil
}

// Register installs browser-login, explicit claim, and read-only catalog
// routes. The context, config and logger must be the same startup values used
// to provide client.
func Register(ctx context.Context, config cfg.Config, logger log.Logger, router *httpserver.Router, ops claims.Operations, identities auth.Authenticator, client sqlc.Client) error {
	if ctx == nil || router == nil || ops == nil || identities == nil || client == nil {
		return errors.New("api: context, router, service, authenticator, and client are required")
	}
	var authSettings auth.Settings
	if config != nil {
		if err := config.UnmarshalKey("claimy.auth", &authSettings); err != nil {
			return fmt.Errorf("api: read auth settings: %w", err)
		}
	}
	if err := auth.ValidateCLISettings(authSettings); err != nil {
		return fmt.Errorf("api: validate auth CLI settings: %w", err)
	}
	h := &service{ops: ops, identities: identities, login: authSettings}
	router.GET("/v1/auth/config", h.loginConfig)
	router.GET("/v1/auth/me", h.loginIdentity)
	router.POST("/v1/claims/acquire", h.acquire)
	router.POST("/v1/claims/query", h.query)
	router.POST("/v1/claims/:id/release", h.release)
	router.PATCH("/v1/claims/:id", h.expiry)
	if err := registerCatalog(ctx, config, logger, router, client, identities); err != nil {
		return err
	}

	return nil
}

func (s *service) loginConfig(c *gin.Context) {
	if !s.login.CLI.Enabled {
		writeError(c, claims.NewError(claims.LoginDisabled, "browser login is disabled"))

		return
	}
	var params *map[string]string
	if len(s.login.CLI.AuthorizationParams) > 0 {
		params = &s.login.CLI.AuthorizationParams
	}
	writeJSON(c, http.StatusOK, client.LoginConfig{
		Issuer:              s.login.REST.Issuer,
		ClientId:            s.login.CLI.ClientID,
		Scopes:              s.login.CLI.Scopes,
		AuthorizationParams: params,
	})
}

func (s *service) loginIdentity(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	if actor.Channel != claims.REST {
		writeError(c, claims.NewError(claims.Forbidden, "manual identity required"))

		return
	}
	writeJSON(c, http.StatusOK, client.LoginIdentity{Email: openapi_types.Email(actor.Email)})
}

func (s *service) actor(c *gin.Context) (claims.Actor, bool) {
	authz := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(authz) < 8 || !strings.EqualFold(authz[:7], "Bearer ") || strings.TrimSpace(authz[7:]) == "" {
		writeError(c, claims.NewError(claims.Unauthenticated, "authentication required"))

		return claims.Actor{}, false
	}
	a, err := s.identities.REST(c.Request.Context(), strings.TrimSpace(authz[7:]))
	if err != nil {
		var ce *claims.Error
		if errors.As(err, &ce) {
			switch ce.Code {
			case claims.Forbidden, claims.StorageError:
				writeError(c, err)
			default:
				writeError(c, claims.NewError(claims.Unauthenticated, "authentication failed"))
			}
		} else {
			writeError(c, claims.NewError(claims.Unauthenticated, "authentication failed"))
		}

		return claims.Actor{}, false
	}

	return a, true
}

func decode(c *gin.Context, dst any) error {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return claims.NewError(claims.Invalid, "invalid JSON request")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil {
		if value, ok := fields["app"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			c.Set("json.app.null", true)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return claims.NewError(claims.Invalid, "invalid JSON request")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return claims.NewError(claims.Invalid, "request must contain one JSON object")
	}

	return nil
}

func (s *service) acquire(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in client.AcquireRequest
	if err := decode(c, &in); err != nil {
		writeError(c, err)

		return
	}
	scope := claims.Scope{Group: in.Group}
	if _, ok := c.Get("json.app.null"); ok {
		writeError(c, claims.NewError(claims.Invalid, "app must not be null"))

		return
	}
	if in.App != nil {
		if strings.TrimSpace(*in.App) == "" {
			writeError(c, claims.NewError(claims.Invalid, "app must not be empty"))

			return
		}
		scope.App = *in.App
	}
	req := claims.AcquireRequest{Scope: scope, Environments: make([]claims.Environment, len(in.Environments)), RequestID: in.RequestId}
	for i, e := range in.Environments {
		req.Environments[i] = claims.Environment(e)
	}
	if in.ExpiresAt != nil {
		req.ExpiresAt = in.ExpiresAt
	}
	out, err := s.ops.Acquire(c.Request.Context(), actor, req)
	if err != nil {
		writeError(c, err)

		return
	}
	wireOut, err := wireAcquireResult(out)
	if err != nil {
		writeError(c, claims.NewError(claims.StorageError, "failed to encode acquire response"))

		return
	}
	writeJSON(c, http.StatusOK, wireOut)
}

func (s *service) query(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in client.QueryRequest
	if err := decode(c, &in); err != nil {
		writeError(c, err)

		return
	}
	if _, ok := c.Get("json.app.null"); ok {
		writeError(c, claims.NewError(claims.Invalid, "app must not be null"))

		return
	}
	if strings.TrimSpace(in.Group) == "" {
		writeError(c, claims.NewError(claims.Invalid, "group is required"))

		return
	}
	scope := claims.Scope{Group: in.Group}
	if in.App != nil {
		if strings.TrimSpace(*in.App) == "" {
			writeError(c, claims.NewError(claims.Invalid, "app must not be empty"))

			return
		}
		scope.App = *in.App
	}
	req := claims.QueryRequest{Scope: scope}
	if in.Environments != nil {
		req.Environments = make([]claims.Environment, len(*in.Environments))
		for i, e := range *in.Environments {
			req.Environments[i] = claims.Environment(e)
		}
	}
	if in.At != nil {
		req.At = in.At
	}
	out, err := s.ops.Query(c.Request.Context(), actor, req)
	if err != nil {
		writeError(c, err)

		return
	}
	wireOut, err := wireQueryResult(out)
	if err != nil {
		writeError(c, claims.NewError(claims.StorageError, "failed to encode query response"))

		return
	}
	writeJSON(c, http.StatusOK, wireOut)
}

func (s *service) release(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in client.MutationRequest
	if err := decode(c, &in); err != nil {
		writeError(c, err)

		return
	}
	if in.RequestId == "" {
		writeError(c, claims.NewError(claims.Invalid, "requestId is required"))

		return
	}
	out, err := s.ops.Release(c.Request.Context(), actor, claims.ReleaseRequest{ClaimID: c.Param("id"), RequestID: in.RequestId})
	if err != nil {
		writeError(c, err)

		return
	}
	wireOut, err := wireMutationResult(out)
	if err != nil {
		writeError(c, claims.NewError(claims.StorageError, "failed to encode release response"))

		return
	}
	writeJSON(c, http.StatusOK, wireOut)
}

func (s *service) expiry(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in client.ExpiryRequest
	if err := decode(c, &in); err != nil {
		writeError(c, err)

		return
	}
	if in.ExpectedRevision < 1 || in.ExpectedRevision > int64(^uint32(0)) {
		writeError(c, claims.NewError(claims.Invalid, "expectedRevision must be between 1 and 4294967295"))

		return
	}
	rev := uint32(in.ExpectedRevision)
	out, err := s.ops.ChangeExpiry(c.Request.Context(), actor, claims.ExpiryRequest{ClaimID: c.Param("id"), ExpiresAt: in.ExpiresAt, ExpectedRevision: &rev, RequestID: in.RequestId})
	if err != nil {
		writeError(c, err)

		return
	}
	wireOut, err := wireMutationResult(out)
	if err != nil {
		writeError(c, claims.NewError(claims.StorageError, "failed to encode expiry response"))

		return
	}
	writeJSON(c, http.StatusOK, wireOut)
}

func writeJSON(c *gin.Context, status int, v any) {
	c.Header("Content-Type", "application/json")
	c.JSON(status, v)
}

func writeError(c *gin.Context, err error) {
	status, _ := ErrorMapper(err)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(c, status, client.ErrorResponse{Error: client.ErrorDetail{Code: client.ErrorDetailCode(errorCode(err)), Message: safeMessage(err), Status: int32(status)}})
}

func errorCode(err error) claims.ErrorCode {
	var ce *claims.Error
	if errors.As(err, &ce) {
		return ce.Code
	}

	return claims.StorageError
}

func safeMessage(err error) string {
	var ce *claims.Error
	if errors.As(err, &ce) && ce.Code != claims.StorageError {
		return ce.Message
	}

	return "internal server error"
}

// ErrorMapper maps typed claim errors to HTTP status codes.
func ErrorMapper(err error) (int, bool) {
	var ce *claims.Error
	if !errors.As(err, &ce) {
		return 0, false
	}
	switch ce.Code {
	case claims.Invalid:
		return 400, true
	case claims.Unauthenticated:
		return 401, true
	case claims.Forbidden:
		return 403, true
	case claims.NotFound, claims.LoginDisabled:
		return 404, true
	case claims.HistoryUnavailable:
		return 410, true
	case claims.ConflictError:
		return 409, true
	case claims.StorageError:
		return 500, true
	default:
		return 0, false
	}
}

// ErrorHandler returns the API error payload for a typed claim error.
func ErrorHandler(status int, err error) any {
	return client.ErrorResponse{Error: client.ErrorDetail{Code: client.ErrorDetailCode(errorCode(err)), Message: safeMessage(err), Status: int32(status)}}
}
