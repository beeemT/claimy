// Package client provides a typed Go client for Claimy's HTTP API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultHTTPTimeout = 30 * time.Second

// API is the public Claimy HTTP client. Its methods use the generated
// transport and wire models from this package.
type API struct {
	client *ClientWithResponses
}

// New constructs a Claimy API client for server. Unless overridden with
// WithHTTPClient, requests use a finite thirty-second timeout.
func New(server string, opts ...ClientOption) (*API, error) {
	if strings.TrimSpace(server) == "" {
		return nil, errors.New("client: server URL is required")
	}

	allOpts := make([]ClientOption, 0, len(opts)+1)
	allOpts = append(allOpts, WithHTTPClient(&http.Client{Timeout: defaultHTTPTimeout}))
	allOpts = append(allOpts, opts...)
	generated, err := NewClientWithResponses(server, allOpts...)
	if err != nil {
		return nil, fmt.Errorf("client: create API client: %w", err)
	}

	return &API{client: generated}, nil
}

// WithBearerToken adds a bearer token to every request. Tokens must be
// non-blank and must not contain carriage returns or line feeds.
func WithBearerToken(token string) ClientOption {
	return func(c *Client) error {
		if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
			return errors.New("client: bearer token must be non-blank and must not contain CR or LF")
		}
		if c == nil {
			return errors.New("client: bearer token option received a nil client")
		}

		return WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)

			return nil
		})(c)
	}
}

// APIError describes a non-successful Claimy HTTP response.
type APIError struct {
	StatusCode int
	Code       ErrorDetailCode
	Message    string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		if e.StatusCode > 0 {
			return fmt.Sprintf("claimy API request failed with HTTP status %d", e.StatusCode)
		}

		return "claimy API request failed"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("claimy API request failed with HTTP status %d: %s", e.StatusCode, e.Message)
	}

	return "claimy API request failed: " + e.Message
}

func (a *API) generated() (*ClientWithResponses, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("client: API is nil")
	}

	return a.client, nil
}

func requestFailure(operation string, err error) error {
	return fmt.Errorf("client: %s request failed: %w", operation, err)
}

func selectErrorPayload(status int, badRequest, unauthorized, forbidden, notFound, conflict, historyUnavailable, serverError *ErrorResponse) *ErrorResponse {
	switch status {
	case http.StatusBadRequest:
		return badRequest
	case http.StatusUnauthorized:
		return unauthorized
	case http.StatusForbidden:
		return forbidden
	case http.StatusNotFound:
		return notFound
	case http.StatusConflict:
		return conflict
	case http.StatusGone:
		return historyUnavailable
	case http.StatusInternalServerError:
		return serverError
	default:
		return nil
	}
}

func knownErrorCode(code ErrorDetailCode) bool {
	switch code {
	case ErrorDetailCodeConflict, ErrorDetailCodeForbidden, ErrorDetailCodeHistoryUnavailable, ErrorDetailCodeInvalidRequest, ErrorDetailCodeNotFound, ErrorDetailCodeStorageError, ErrorDetailCodeUnauthenticated, ErrorDetailCodeLoginDisabled:
		return true
	default:
		return false
	}
}

func fallbackErrorCode(status int) ErrorDetailCode {
	switch status {
	case http.StatusUnauthorized:
		return ErrorDetailCodeUnauthenticated
	case http.StatusForbidden:
		return ErrorDetailCodeForbidden
	case http.StatusNotFound:
		return ErrorDetailCodeNotFound
	case http.StatusGone:
		return ErrorDetailCodeHistoryUnavailable
	case http.StatusConflict:
		return ErrorDetailCodeConflict
	case http.StatusBadRequest:
		return ErrorDetailCodeInvalidRequest
	default:
		if status >= http.StatusInternalServerError {
			return ErrorDetailCodeStorageError
		}

		return ErrorDetailCodeInvalidRequest
	}
}

func responseFailure(response *http.Response, body []byte, payload *ErrorResponse) error {
	if response == nil {
		return errors.New("client: response is missing HTTP metadata")
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}

	code := fallbackErrorCode(response.StatusCode)
	message := http.StatusText(response.StatusCode)
	if message == "" {
		message = "request failed"
	}
	if payload != nil && knownErrorCode(payload.Error.Code) {
		code = payload.Error.Code
		if strings.TrimSpace(payload.Error.Message) != "" {
			message = payload.Error.Message
		}
	}
	if payload == nil && len(body) > 0 {
		var decoded ErrorResponse
		if err := json.Unmarshal(body, &decoded); err == nil && knownErrorCode(decoded.Error.Code) {
			code = decoded.Error.Code
			if strings.TrimSpace(decoded.Error.Message) != "" {
				message = decoded.Error.Message
			}
		}
	}

	return &APIError{StatusCode: response.StatusCode, Code: code, Message: message}
}

func responseBodyError(operation string, response *http.Response, body []byte) error {
	if response == nil {
		return errors.New("client: response is missing HTTP metadata")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("client: %s response body is empty", operation)
	}

	return fmt.Errorf("client: %s response shape is invalid", operation)
}

func validateClaimActiveNow(operation string, raw json.RawMessage) error {
	var claim struct {
		ActiveNow *bool `json:"activeNow"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil || claim.ActiveNow == nil {
		return fmt.Errorf("client: %s response is missing activeNow", operation)
	}

	return nil
}

func validateAcquireResponse(body []byte, response *AcquireResponse) error {
	var envelope struct {
		Acquired  *bool              `json:"acquired"`
		Claim     json.RawMessage    `json:"claim"`
		Conflicts *[]json.RawMessage `json:"conflicts"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Acquired == nil {
		return errors.New("client: acquire response is missing acquired")
	}
	if *envelope.Acquired != response.Acquired {
		return errors.New("client: acquire response acquired field is inconsistent")
	}
	if response.Acquired {
		if response.Claim == nil || response.Claim.Id == uuid.Nil {
			return errors.New("client: acquire response has no claim identifier")
		}
		if len(bytes.TrimSpace(envelope.Claim)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Claim), []byte("null")) {
			return errors.New("client: acquire response is missing claim")
		}

		return validateClaimActiveNow("acquire claim", envelope.Claim)
	}
	if envelope.Conflicts == nil || len(*envelope.Conflicts) == 0 || response.Conflicts == nil || len(*response.Conflicts) == 0 {
		return errors.New("client: acquire response is missing conflicts for a busy result")
	}

	return nil
}

func validateQueryResponse(body []byte) error {
	var envelope struct {
		Claims *[]json.RawMessage `json:"claims"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Claims == nil {
		return errors.New("client: query response is missing claims")
	}
	for _, claim := range *envelope.Claims {
		if err := validateClaimActiveNow("query claim", claim); err != nil {
			return err
		}
	}

	return nil
}

func validateMutationResponse(body []byte, response *MutationResponse, operation string) error {
	var envelope struct {
		Claim json.RawMessage `json:"claim"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(bytes.TrimSpace(envelope.Claim)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Claim), []byte("null")) {
		return fmt.Errorf("client: %s response is missing claim", operation)
	}
	if response.Claim.Id == uuid.Nil {
		return fmt.Errorf("client: %s response has no claim identifier", operation)
	}

	return validateClaimActiveNow(operation+" claim", envelope.Claim)
}

func validateLoginConfigResponse(body []byte) error {
	var envelope struct {
		Issuer   *string   `json:"issuer"`
		ClientID *string   `json:"clientId"`
		Scopes   *[]string `json:"scopes"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return errors.New("client: login config response is missing required fields")
	}
	if envelope.Issuer == nil || envelope.ClientID == nil || envelope.Scopes == nil || len(*envelope.Scopes) == 0 {
		return errors.New("client: login config response is missing required fields")
	}
	if strings.TrimSpace(*envelope.Issuer) == "" || strings.TrimSpace(*envelope.ClientID) == "" {
		return errors.New("client: login config response is missing required fields")
	}

	return nil
}

func validateLoginIdentityResponse(body []byte) error {
	var envelope struct {
		Email *string `json:"email"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Email == nil || strings.TrimSpace(*envelope.Email) == "" {
		return errors.New("client: login identity response is missing email")
	}

	return nil
}

// Acquire acquires the requested environments or returns a valid busy result.
func (a *API) Acquire(ctx context.Context, request AcquireRequest) (*AcquireResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.AcquireClaimWithResponse(ctx, request)
	if err != nil {
		return nil, requestFailure("acquire", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, nil, response.JSON409, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, responseBodyError("acquire", response.HTTPResponse, response.Body)
	}
	if err := validateAcquireResponse(response.Body, response.JSON200); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// Query reads the claim state for a scope and optional point in time.
func (a *API) Query(ctx context.Context, request QueryRequest) (*QueryResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.QueryClaimsWithResponse(ctx, request)
	if err != nil {
		return nil, requestFailure("query", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, response.JSON404, nil, response.JSON410, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, responseBodyError("query", response.HTTPResponse, response.Body)
	}
	if err := validateQueryResponse(response.Body); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// Release releases a claim by UUID.
func (a *API) Release(ctx context.Context, id string, request MutationRequest) (*MutationResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("client: claim id must be a valid UUID")
	}
	response, err := generated.ReleaseClaimWithResponse(ctx, parsed, request)
	if err != nil {
		return nil, requestFailure("release", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, response.JSON404, response.JSON409, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, responseBodyError("release", response.HTTPResponse, response.Body)
	}
	if err := validateMutationResponse(response.Body, response.JSON200, "release"); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// ChangeExpiry updates a claim expiry by UUID.
func (a *API) ChangeExpiry(ctx context.Context, id string, request ExpiryRequest) (*MutationResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("client: claim id must be a valid UUID")
	}
	response, err := generated.ChangeClaimExpiryWithResponse(ctx, parsed, request)
	if err != nil {
		return nil, requestFailure("change expiry", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, response.JSON404, response.JSON409, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, responseBodyError("change expiry", response.HTTPResponse, response.Body)
	}
	if err := validateMutationResponse(response.Body, response.JSON200, "change expiry"); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// LoginConfig returns the public browser-login configuration.
func (a *API) LoginConfig(ctx context.Context) (*LoginConfig, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.GetLoginConfigWithResponse(ctx)
	if err != nil {
		return nil, requestFailure("login config", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), nil, nil, nil, response.JSON404, nil, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil || bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) {
		return nil, responseBodyError("login config", response.HTTPResponse, response.Body)
	}
	if err := validateLoginConfigResponse(response.Body); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// Identity returns the authenticated manual REST identity.
func (a *API) Identity(ctx context.Context) (*LoginIdentity, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.GetLoginIdentityWithResponse(ctx)
	if err != nil {
		return nil, requestFailure("login identity", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), nil, response.JSON401, response.JSON403, nil, nil, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil || bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) {
		return nil, responseBodyError("login identity", response.HTTPResponse, response.Body)
	}
	if err := validateLoginIdentityResponse(response.Body); err != nil {
		return nil, err
	}

	return response.JSON200, nil
}

// ListGroups lists catalog groups.
func (a *API) ListGroups(ctx context.Context, request CatalogListRequest) (*CatalogGroupsResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.ListCatalogGroupsWithResponse(ctx, request)
	if err != nil {
		return nil, requestFailure("list catalog groups", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, nil, nil, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil || bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) {
		return nil, responseBodyError("list catalog groups", response.HTTPResponse, response.Body)
	}

	return response.JSON200, nil
}

// GetGroup retrieves a catalog group by canonical name.
func (a *API) GetGroup(ctx context.Context, group string) (*CatalogGroup, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.GetCatalogGroupWithResponse(ctx, group)
	if err != nil {
		return nil, requestFailure("get catalog group", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, response.JSON404, nil, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil || bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) {
		return nil, responseBodyError("get catalog group", response.HTTPResponse, response.Body)
	}

	return response.JSON200, nil
}

// ListApps lists apps in a catalog group.
func (a *API) ListApps(ctx context.Context, group string, request CatalogListRequest) (*CatalogAppsResponse, error) {
	generated, err := a.generated()
	if err != nil {
		return nil, err
	}
	response, err := generated.ListCatalogAppsWithResponse(ctx, group, request)
	if err != nil {
		return nil, requestFailure("list catalog apps", err)
	}
	if err := responseFailure(response.HTTPResponse, response.Body, selectErrorPayload(response.StatusCode(), response.JSON400, response.JSON401, response.JSON403, response.JSON404, nil, nil, response.JSON500)); err != nil {
		return nil, err
	}
	if response.JSON200 == nil || bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) {
		return nil, responseBodyError("list catalog apps", response.HTTPResponse, response.Body)
	}

	return response.JSON200, nil
}
