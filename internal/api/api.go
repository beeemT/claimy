// Package api provides authenticated HTTP handlers for claim operations.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	"github.com/gin-gonic/gin"
	"github.com/gosoline-project/httpserver"
	"github.com/gosoline-project/sqlc"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
)

type service struct {
	ops        claims.Operations
	identities auth.Authenticator
}

// Register installs explicit claim routes and read-only catalog routes. The context,
// config and logger must be the same startup values used to provide client.
func Register(ctx context.Context, config cfg.Config, logger log.Logger, router *httpserver.Router, ops claims.Operations, identities auth.Authenticator, client sqlc.Client) error {
	if ctx == nil || router == nil || ops == nil || identities == nil || client == nil {
		return errors.New("api: context, router, service, authenticator, and client are required")
	}
	h := &service{ops: ops, identities: identities}
	router.POST("/v1/claims/acquire", h.acquire)
	router.POST("/v1/claims/query", h.query)
	router.POST("/v1/claims/:id/release", h.release)
	router.PATCH("/v1/claims/:id", h.expiry)
	if err := registerCatalog(ctx, config, logger, router, client, identities); err != nil {
		return err
	}

	return nil
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
	var in AcquireRequest
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
	writeJSON(c, http.StatusOK, out)
}

func (s *service) query(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in QueryRequest
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
	writeJSON(c, http.StatusOK, out)
}

func (s *service) release(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in MutationRequest
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
	writeJSON(c, http.StatusOK, out)
}

func (s *service) expiry(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var in ExpiryRequest
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
	writeJSON(c, http.StatusOK, out)
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
	writeJSON(c, status, ErrorResponse{Error: ErrorDetail{Code: ErrorDetailCode(errorCode(err)), Message: safeMessage(err), Status: int32(status)}})
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
	case claims.NotFound:
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
	return ErrorResponse{Error: ErrorDetail{Code: ErrorDetailCode(errorCode(err)), Message: safeMessage(err), Status: int32(status)}}
}
