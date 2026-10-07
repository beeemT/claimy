package claims

import (
	"context"
	"time"
)

// Environment identifies a deployment environment for a claim.
type Environment string

// Environment constants name the supported deployment environments.
const (
	Sandbox   Environment = "sandbox"
	Prod      Environment = "prod"
	Retention             = 90 * 24 * time.Hour
)

// Channel identifies the authenticated source of an actor.
type Channel string

// Channel constants name the supported authentication channels.
const (
	REST       Channel = "rest"
	GitLabCI   Channel = "gitlab_ci"
	GoogleChat Channel = "google_chat"
)

// Source identifies how a claim was created.
type Source string

// Source constants name the supported claim origins.
const (
	Manual Source = "manual"
	CI     Source = "ci"
)

// GitLabIdentity records the GitLab project and job that authenticated an actor.
type GitLabIdentity struct {
	Issuer    string `json:"issuer"`
	ProjectID string `json:"projectId"`
	JobID     string `json:"jobId"`
	UserID    string `json:"userId"`
}

// Actor contains only identity established by an authentication adapter.
type Actor struct {
	Email   string
	Issuer  string
	Subject string
	Channel Channel
	GitLab  *GitLabIdentity
}

// Principal is the stable identity key used for mutation replay protection.
type Principal struct {
	Kind   string
	Issuer string
	ID     string
}

// Scope identifies a claim's group and optional app.
type Scope struct {
	Group string `json:"group"`
	App   string `json:"app,omitempty"`
}

// Claim records ownership, scope, environment, and lifecycle state.
type Claim struct {
	ID           string          `json:"id"`
	Scope        Scope           `json:"scope"`
	Environments []Environment   `json:"environments"`
	OwnerEmail   string          `json:"ownerEmail"`
	Source       Source          `json:"source"`
	GitLab       *GitLabIdentity `json:"gitlab,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	ExpiresAt    time.Time       `json:"expiresAt"`
	ReleasedAt   *time.Time      `json:"releasedAt,omitempty"`
	Revision     uint32          `json:"revision"`
	ActiveNow    bool            `json:"activeNow"`
	Inherited    bool            `json:"inherited"`
}

// Conflict describes an active claim that blocks acquisition.
type Conflict struct {
	ID           string        `json:"id"`
	Scope        Scope         `json:"scope"`
	Environments []Environment `json:"environments"`
	OwnerEmail   string        `json:"ownerEmail"`
	Source       Source        `json:"source"`
	ExpiresAt    time.Time     `json:"expiresAt"`
}

// MutationMeta is populated by Service, never by an HTTP or Chat body.
type MutationMeta struct {
	Principal   Principal `json:"-"`
	PayloadHash [32]byte  `json:"-"`
}

// AcquireRequest contains the scope, environments, and optional expiry to claim.
type AcquireRequest struct {
	Scope        Scope         `json:"scope"`
	Environments []Environment `json:"environments"`
	ExpiresAt    *time.Time    `json:"expiresAt,omitempty"`
	RequestID    string        `json:"requestId"`
	MutationMeta
}

// AcquireResult reports whether a claim was acquired and any blocking claims.
type AcquireResult struct {
	Acquired  bool       `json:"acquired"`
	Claim     *Claim     `json:"claim,omitempty"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// QueryRequest contains claim filters. An empty Group selects all groups for Chat's list operation.
type QueryRequest struct {
	Scope        Scope         `json:"scope"`
	Environments []Environment `json:"environments"`
	At           *time.Time    `json:"at,omitempty"`
}

// QueryResult contains the filtered claims and availability at the selected time.
type QueryResult struct {
	Known            bool      `json:"known"`
	At               time.Time `json:"at"`
	Projected        bool      `json:"projected"`
	Free             bool      `json:"free"`
	AllowedForCaller bool      `json:"allowedForCaller"`
	Claims           []Claim   `json:"claims"`
}

// ReleaseRequest identifies the claim to release.
type ReleaseRequest struct {
	ClaimID   string `json:"claimId"`
	RequestID string `json:"requestId"`
	MutationMeta
}

// ExpiryRequest contains the claim expiry and expected revision for an update.
type ExpiryRequest struct {
	ClaimID   string    `json:"claimId"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Nil is permitted only for Chat; resolve it under the group lock after replay lookup.
	ExpectedRevision *uint32 `json:"expectedRevision,omitempty"`
	RequestID        string  `json:"requestId"`
	MutationMeta
}

// MutationResult reports whether a claim changed and its resulting state.
type MutationResult struct {
	Changed bool  `json:"changed"`
	Claim   Claim `json:"claim"`
}

// PruneResult counts the retained records removed by pruning.
type PruneResult struct {
	Results  int64
	Versions int64
	Claims   int64
}

// Store receives canonical, validated requests and owns atomic persistence.
type Store interface {
	Acquire(context.Context, Actor, AcquireRequest) (AcquireResult, error)
	Query(context.Context, Actor, QueryRequest) (QueryResult, error)
	Release(context.Context, Actor, ReleaseRequest) (MutationResult, error)
	ChangeExpiry(context.Context, Actor, ExpiryRequest) (MutationResult, error)
	Prune(context.Context) (PruneResult, error)
}

// Operations is the shared boundary used by REST and Chat adapters.
type Operations interface {
	Acquire(context.Context, Actor, AcquireRequest) (AcquireResult, error)
	Query(context.Context, Actor, QueryRequest) (QueryResult, error)
	Release(context.Context, Actor, ReleaseRequest) (MutationResult, error)
	ChangeExpiry(context.Context, Actor, ExpiryRequest) (MutationResult, error)
}

// ErrorCode identifies a stable domain failure category.
type ErrorCode string

// Error codes identify the stable domain failure categories.
const (
	Invalid            ErrorCode = "invalid_request"
	Unauthenticated    ErrorCode = "unauthenticated"
	Forbidden          ErrorCode = "forbidden"
	NotFound           ErrorCode = "not_found"
	HistoryUnavailable ErrorCode = "history_unavailable"
	ConflictError      ErrorCode = "conflict"
	StorageError       ErrorCode = "storage_error"
	LoginDisabled      ErrorCode = "login_disabled"
)

// Error carries a domain code, a safe message, and an optional cause.
type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

// NewError creates a typed domain error with the given code and message.
func NewError(code ErrorCode, message string) error {
	return &Error{Code: code, Message: message}
}
