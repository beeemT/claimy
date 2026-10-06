package claims

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Service validates and canonicalizes domain requests before delegating atomic work to Store.
type Service struct {
	store Store
}

// NewService constructs the domain service over its persistence boundary.
func NewService(store Store) *Service {
	return &Service{store: store}
}

var _ Operations = (*Service)(nil)

// Acquire validates a claim request and attaches idempotency data derived from the authenticated actor.
func (s *Service) Acquire(ctx context.Context, actor Actor, request AcquireRequest) (AcquireResult, error) {
	if err := s.requireStore(); err != nil {
		return AcquireResult{}, err
	}
	canonical, principal, err := canonicalMutationActor(actor)
	if err != nil {
		return AcquireResult{}, err
	}

	group, err := CanonicalSlug(request.Scope.Group)
	if err != nil {
		return AcquireResult{}, err
	}
	app := ""
	if request.Scope.App != "" {
		app, err = CanonicalSlug(request.Scope.App)
		if err != nil {
			return AcquireResult{}, err
		}
	}
	environments, err := canonicalEnvironments(request.Environments, false)
	if err != nil {
		return AcquireResult{}, err
	}
	requestID, err := canonicalRequestID(request.RequestID)
	if err != nil {
		return AcquireResult{}, err
	}

	var expiry *time.Time
	expiryMode := "default"
	var expiryHash string
	if request.ExpiresAt != nil {
		if request.ExpiresAt.IsZero() {
			return AcquireResult{}, invalid("expiresAt must be a valid timestamp")
		}
		normalized := canonicalTime(*request.ExpiresAt)
		expiry = &normalized
		expiryMode = "explicit"
		expiryHash = normalized.Format(time.RFC3339Nano)
	}

	payloadHash, err := hashPayload(struct {
		Operation    string        `json:"operation"`
		Scope        Scope         `json:"scope"`
		Environments []Environment `json:"environments"`
		ExpiryMode   string        `json:"expiryMode"`
		ExpiresAt    string        `json:"expiresAt,omitempty"`
	}{
		Operation:    "acquire/v1",
		Scope:        Scope{Group: group, App: app},
		Environments: environments,
		ExpiryMode:   expiryMode,
		ExpiresAt:    expiryHash,
	})
	if err != nil {
		return AcquireResult{}, err
	}

	return s.store.Acquire(ctx, canonical, AcquireRequest{
		Scope:        Scope{Group: group, App: app},
		Environments: environments,
		ExpiresAt:    expiry,
		RequestID:    requestID,
		MutationMeta: MutationMeta{Principal: principal, PayloadHash: payloadHash},
	})
}

// Query canonicalizes read filters without registering unknown resources.
func (s *Service) Query(ctx context.Context, actor Actor, request QueryRequest) (QueryResult, error) {
	if err := s.requireStore(); err != nil {
		return QueryResult{}, err
	}
	canonical, _, err := canonicalMutationActor(actor)
	if err != nil {
		return QueryResult{}, err
	}

	group := ""
	if request.Scope.Group == "" {
		if canonical.Channel != GoogleChat || request.Scope.App != "" {
			return QueryResult{}, invalid("group is required for this query")
		}
	} else {
		group, err = CanonicalSlug(request.Scope.Group)
		if err != nil {
			return QueryResult{}, err
		}
	}
	app := ""
	if request.Scope.App != "" {
		if group == "" {
			return QueryResult{}, invalid("app requires a group")
		}
		app, err = CanonicalSlug(request.Scope.App)
		if err != nil {
			return QueryResult{}, err
		}
	}
	environments, err := canonicalEnvironments(request.Environments, true)
	if err != nil {
		return QueryResult{}, err
	}
	var at *time.Time
	if request.At != nil {
		if request.At.IsZero() {
			return QueryResult{}, invalid("at must be a valid timestamp")
		}
		normalized := canonicalTime(*request.At)
		at = &normalized
	}

	return s.store.Query(ctx, canonical, QueryRequest{
		Scope:        Scope{Group: group, App: app},
		Environments: environments,
		At:           at,
	})
}

// Release validates one claim identifier and attaches actor-derived replay metadata.
func (s *Service) Release(ctx context.Context, actor Actor, request ReleaseRequest) (MutationResult, error) {
	if err := s.requireStore(); err != nil {
		return MutationResult{}, err
	}
	canonical, principal, err := canonicalMutationActor(actor)
	if err != nil {
		return MutationResult{}, err
	}
	claimID, err := canonicalClaimID(request.ClaimID)
	if err != nil {
		return MutationResult{}, err
	}
	requestID, err := canonicalRequestID(request.RequestID)
	if err != nil {
		return MutationResult{}, err
	}
	payloadHash, err := hashPayload(struct {
		Operation string `json:"operation"`
		ClaimID   string `json:"claimId"`
	}{Operation: "release/v1", ClaimID: claimID})
	if err != nil {
		return MutationResult{}, err
	}

	return s.store.Release(ctx, canonical, ReleaseRequest{
		ClaimID:      claimID,
		RequestID:    requestID,
		MutationMeta: MutationMeta{Principal: principal, PayloadHash: payloadHash},
	})
}

// ChangeExpiry validates an expiry update and attaches actor-derived replay metadata.
func (s *Service) ChangeExpiry(ctx context.Context, actor Actor, request ExpiryRequest) (MutationResult, error) {
	if err := s.requireStore(); err != nil {
		return MutationResult{}, err
	}
	canonical, principal, err := canonicalMutationActor(actor)
	if err != nil {
		return MutationResult{}, err
	}
	claimID, err := canonicalClaimID(request.ClaimID)
	if err != nil {
		return MutationResult{}, err
	}
	requestID, err := canonicalRequestID(request.RequestID)
	if err != nil {
		return MutationResult{}, err
	}
	if request.ExpiresAt.IsZero() {
		return MutationResult{}, invalid("expiresAt must be a valid timestamp")
	}
	if request.ExpectedRevision == nil && canonical.Channel != GoogleChat {
		return MutationResult{}, invalid("expectedRevision is required outside Chat")
	}
	expiry := canonicalTime(request.ExpiresAt)
	payloadHash, err := hashPayload(struct {
		Operation        string  `json:"operation"`
		ClaimID          string  `json:"claimId"`
		ExpiresAt        string  `json:"expiresAt"`
		ExpectedRevision *uint32 `json:"expectedRevision,omitempty"`
	}{
		Operation:        "change_expiry/v1",
		ClaimID:          claimID,
		ExpiresAt:        expiry.Format(time.RFC3339Nano),
		ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		return MutationResult{}, err
	}

	return s.store.ChangeExpiry(ctx, canonical, ExpiryRequest{
		ClaimID:          claimID,
		ExpiresAt:        expiry,
		ExpectedRevision: cloneRevision(request.ExpectedRevision),
		RequestID:        requestID,
		MutationMeta:     MutationMeta{Principal: principal, PayloadHash: payloadHash},
	})
}

func (s *Service) requireStore() error {
	if s == nil || s.store == nil {
		return &Error{Code: StorageError, Message: "claim store is unavailable"}
	}

	return nil
}

func canonicalTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func cloneRevision(revision *uint32) *uint32 {
	if revision == nil {
		return nil
	}
	cloned := *revision

	return &cloned
}

func hashPayload(payload any) ([32]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [32]byte{}, fmt.Errorf("marshal canonical claim payload: %w", err)
	}

	return sha256.Sum256(encoded), nil
}

func invalid(message string) error {
	return &Error{Code: Invalid, Message: message}
}

func canonicalEnvironments(input []Environment, emptyMeansBoth bool) ([]Environment, error) {
	if len(input) == 0 {
		if !emptyMeansBoth {
			return nil, invalid("at least one environment is required")
		}

		return []Environment{Prod, Sandbox}, nil
	}
	if len(input) > 2 {
		return nil, invalid("at most two environments may be selected")
	}
	canonical := make([]Environment, 0, len(input))
	seen := make(map[Environment]struct{}, len(input))
	for _, environment := range input {
		value := Environment(strings.ToLower(strings.TrimSpace(string(environment))))
		if value != Sandbox && value != Prod {
			return nil, invalid("environment must be sandbox or prod")
		}
		if _, exists := seen[value]; exists {
			return nil, invalid("environment selection contains a duplicate")
		}
		seen[value] = struct{}{}
		canonical = append(canonical, value)
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i] < canonical[j] })

	return canonical, nil
}
