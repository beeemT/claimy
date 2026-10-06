package claims_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/internal/claims/mocks"
	testifymock "github.com/stretchr/testify/mock"
)

type recordedStoreCalls struct {
	acquireActors []claims.Actor
	acquireCalls  []claims.AcquireRequest
	queryCalls    []claims.QueryRequest
	releaseCalls  []claims.ReleaseRequest
	expiryCalls   []claims.ExpiryRequest
}

func newRecordingStore(t *testing.T) (*mocks.MockStore, *recordedStoreCalls) {
	t.Helper()
	store := mocks.NewMockStore(t)
	calls := &recordedStoreCalls{}
	store.EXPECT().Acquire(testifymock.Anything, testifymock.Anything, testifymock.Anything).
		RunAndReturn(func(_ context.Context, actor claims.Actor, request claims.AcquireRequest) (claims.AcquireResult, error) {
			calls.acquireActors = append(calls.acquireActors, actor)
			calls.acquireCalls = append(calls.acquireCalls, request)

			return claims.AcquireResult{}, nil
		}).Maybe()
	store.EXPECT().Query(testifymock.Anything, testifymock.Anything, testifymock.Anything).
		RunAndReturn(func(_ context.Context, _ claims.Actor, request claims.QueryRequest) (claims.QueryResult, error) {
			calls.queryCalls = append(calls.queryCalls, request)

			return claims.QueryResult{}, nil
		}).Maybe()
	store.EXPECT().Release(testifymock.Anything, testifymock.Anything, testifymock.Anything).
		RunAndReturn(func(_ context.Context, _ claims.Actor, request claims.ReleaseRequest) (claims.MutationResult, error) {
			calls.releaseCalls = append(calls.releaseCalls, request)

			return claims.MutationResult{}, nil
		}).Maybe()
	store.EXPECT().ChangeExpiry(testifymock.Anything, testifymock.Anything, testifymock.Anything).
		RunAndReturn(func(_ context.Context, _ claims.Actor, request claims.ExpiryRequest) (claims.MutationResult, error) {
			calls.expiryCalls = append(calls.expiryCalls, request)

			return claims.MutationResult{}, nil
		}).Maybe()

	return store, calls
}

func restActor() claims.Actor {
	return claims.Actor{Email: " Alice+Ops@Example.COM ", Issuer: "https://identity.example", Subject: "user-123", Channel: claims.REST}
}

func requireDomainError(t *testing.T, err error, code claims.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var domainErr *claims.Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected *claims.Error, got %T: %v", err, err)
	}
	if domainErr.Code != code {
		t.Fatalf("error code = %q, want %q", domainErr.Code, code)
	}
}

func TestServiceCanonicalizesAcquisitionAndDistinguishesDefaultExpiry(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	firstExpiry := time.Date(2026, time.January, 2, 12, 30, 0, 123456789, time.FixedZone("offset", 2*60*60))
	secondExpiry := time.Date(2026, time.January, 2, 10, 30, 0, 123456000, time.UTC)
	for _, request := range []claims.AcquireRequest{
		{
			Scope:        claims.Scope{Group: " My-Group ", App: " APP "},
			Environments: []claims.Environment{" SANDBOX ", "PROD"},
			ExpiresAt:    &firstExpiry,
			RequestID:    "request-1",
			MutationMeta: claims.MutationMeta{
				Principal:   claims.Principal{Kind: "forged", Issuer: "attacker", ID: "attacker"},
				PayloadHash: sha256.Sum256([]byte("forged")),
			},
		},
		{
			Scope:        claims.Scope{Group: "my-group", App: "app"},
			Environments: []claims.Environment{claims.Prod, claims.Sandbox},
			ExpiresAt:    &secondExpiry,
			RequestID:    "request-1",
		},
	} {
		if _, err := service.Acquire(context.Background(), restActor(), request); err != nil {
			t.Fatal(err)
		}
	}
	first, second := calls.acquireCalls[0], calls.acquireCalls[1]
	if first.Scope != (claims.Scope{Group: "my-group", App: "app"}) || !reflect.DeepEqual(first.Environments, []claims.Environment{claims.Prod, claims.Sandbox}) {
		t.Fatalf("canonical acquisition scope/environments = %#v / %#v", first.Scope, first.Environments)
	}
	if first.ExpiresAt == nil || second.ExpiresAt == nil || !first.ExpiresAt.Equal(*second.ExpiresAt) || first.ExpiresAt.Location() != time.UTC || first.ExpiresAt.Nanosecond() != 123456000 {
		t.Fatalf("canonical expiry = %#v, want UTC microsecond-normalized equivalent", first.ExpiresAt)
	}
	if first.PayloadHash != second.PayloadHash {
		t.Fatal("equivalent canonical acquisition intents produced different payload hashes")
	}
	if first.Principal != (claims.Principal{Kind: "rest_user", Issuer: "https://identity.example", ID: "user-123"}) {
		t.Fatalf("caller-supplied principal was not replaced: %#v", first.Principal)
	}
	if calls.acquireActors[0].Email != "alice+ops@example.com" {
		t.Fatalf("mutation owner was not canonicalized: %q", calls.acquireActors[0].Email)
	}

	defaultRequest := claims.AcquireRequest{
		Scope: claims.Scope{Group: "my-group", App: "app"}, Environments: []claims.Environment{claims.Sandbox, claims.Prod}, RequestID: "request-default",
	}
	if _, err := service.Acquire(context.Background(), restActor(), defaultRequest); err != nil {
		t.Fatal(err)
	}
	explicitAtDefault, err := claims.DefaultExpiry(time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defaultRequest.ExpiresAt = &explicitAtDefault
	if _, err := service.Acquire(context.Background(), restActor(), defaultRequest); err != nil {
		t.Fatal(err)
	}
	defaultHash := calls.acquireCalls[2].PayloadHash
	explicitHash := calls.acquireCalls[3].PayloadHash
	if defaultHash == explicitHash {
		t.Fatal("omitted/default expiry and an explicit expiry shared a payload hash")
	}
	if calls.acquireCalls[2].ExpiresAt != nil {
		t.Fatal("service resolved default expiry before Store replay lookup")
	}
	ciActor := claims.Actor{
		Email: "ci@example.com", Issuer: "https://gitlab.example", Subject: "user-9", Channel: claims.GitLabCI,
		GitLab: &claims.GitLabIdentity{Issuer: "https://gitlab.example", ProjectID: "42", JobID: "9001", UserID: "user-9"},
	}
	defaultRequest.ExpiresAt = nil
	if _, err := service.Acquire(context.Background(), ciActor, defaultRequest); err != nil {
		t.Fatal(err)
	}
	if calls.acquireCalls[4].ExpiresAt != nil {
		t.Fatal("CI default expiry was resolved outside Store operation time")
	}
}

func TestServiceHashesCanonicalPayloadWithOperationDiscriminator(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	expiry := time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC)
	base := claims.AcquireRequest{Scope: claims.Scope{Group: "alpha"}, Environments: []claims.Environment{claims.Sandbox}, ExpiresAt: &expiry, RequestID: "same-key"}
	changed := base
	changed.Scope.Group = "beta"
	for _, request := range []claims.AcquireRequest{base, changed} {
		if _, err := service.Acquire(context.Background(), restActor(), request); err != nil {
			t.Fatal(err)
		}
	}
	if calls.acquireCalls[0].PayloadHash == calls.acquireCalls[1].PayloadHash {
		t.Fatal("changed group intent reused the original payload hash")
	}

	claimID := "01234567-89ab-cdef-0123-456789abcdef"
	revision := uint32(3)
	if _, err := service.Release(context.Background(), restActor(), claims.ReleaseRequest{ClaimID: strings.ToUpper(claimID), RequestID: "same-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ChangeExpiry(context.Background(), restActor(), claims.ExpiryRequest{
		ClaimID: claimID, ExpiresAt: expiry, ExpectedRevision: &revision, RequestID: "same-key",
	}); err != nil {
		t.Fatal(err)
	}
	release := calls.releaseCalls[0]
	change := calls.expiryCalls[0]
	if release.ClaimID != claimID {
		t.Fatalf("canonical claim ID = %q, want %q", release.ClaimID, claimID)
	}
	if release.PayloadHash == change.PayloadHash {
		t.Fatal("release and expiry-change operations shared a payload hash")
	}
}

func TestServiceMutationMetadataCannotBeSuppliedByRequest(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	forged := claims.MutationMeta{
		Principal:   claims.Principal{Kind: "gitlab_job", Issuer: "forged", ID: "forged"},
		PayloadHash: sha256.Sum256([]byte("forged")),
	}
	request := claims.ReleaseRequest{ClaimID: "01234567-89ab-cdef-0123-456789abcdef", RequestID: "release-1", MutationMeta: forged}
	if _, err := service.Release(context.Background(), restActor(), request); err != nil {
		t.Fatal(err)
	}
	got := calls.releaseCalls[0].MutationMeta
	if got.Principal != (claims.Principal{Kind: "rest_user", Issuer: "https://identity.example", ID: "user-123"}) {
		t.Fatalf("forged principal survived service normalization: %#v", got.Principal)
	}
	if got.PayloadHash == forged.PayloadHash {
		t.Fatal("forged payload hash survived service normalization")
	}
}

func TestServiceNormalizesCustomTimesWithoutClockShortcuts(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	past := time.Date(2020, time.January, 1, 1, 2, 3, 456789123, time.FixedZone("west", -7*60*60))
	if _, err := service.Acquire(context.Background(), restActor(), claims.AcquireRequest{
		Scope: claims.Scope{Group: "group"}, Environments: []claims.Environment{claims.Sandbox}, ExpiresAt: &past, RequestID: "old-expiry",
	}); err != nil {
		t.Fatalf("domain service performed a wall-clock expiry check instead of delegating DB operation-time validation: %v", err)
	}
	want := time.Date(2020, time.January, 1, 8, 2, 3, 456789000, time.UTC)
	got := calls.acquireCalls[0].ExpiresAt
	if got == nil || !got.Equal(want) || got.Location() != time.UTC || got.Nanosecond() != 456789000 {
		t.Fatalf("Store expiry = %#v, want %s UTC", got, want)
	}
	at := past
	if _, err := service.Query(context.Background(), restActor(), claims.QueryRequest{
		Scope: claims.Scope{Group: " GROUP "}, At: &at,
	}); err != nil {
		t.Fatal(err)
	}
	queryAt := calls.queryCalls[0].At
	if queryAt == nil || !queryAt.Equal(want) || queryAt.Location() != time.UTC || queryAt.Nanosecond() != 456789000 {
		t.Fatalf("Store query time = %#v, want %s UTC", queryAt, want)
	}
	revision := uint32(4)
	expiryRequest := claims.ExpiryRequest{
		ClaimID: "01234567-89ab-cdef-0123-456789abcdef", ExpiresAt: past, ExpectedRevision: &revision, RequestID: "expiry-1",
	}
	if _, err := service.ChangeExpiry(context.Background(), restActor(), expiryRequest); err != nil {
		t.Fatalf("domain service performed a wall-clock expiry-edit check instead of delegating DB operation-time validation: %v", err)
	}
	normalizedChange := calls.expiryCalls[0]
	if !normalizedChange.ExpiresAt.Equal(want) || normalizedChange.ExpiresAt.Location() != time.UTC || normalizedChange.ExpiresAt.Nanosecond() != 456789000 {
		t.Fatalf("Store expiry change time = %s, want %s UTC", normalizedChange.ExpiresAt, want)
	}
	equivalentExpiry := want
	expiryRequest.ExpiresAt = equivalentExpiry
	if _, err := service.ChangeExpiry(context.Background(), restActor(), expiryRequest); err != nil {
		t.Fatal(err)
	}
	if normalizedChange.PayloadHash != calls.expiryCalls[1].PayloadHash {
		t.Fatal("equivalent expiry-change timestamps produced different canonical payload hashes")
	}
}

func TestServiceCanonicalizesQueryDefaultsAndChatAllGroups(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	if _, err := service.Query(context.Background(), restActor(), claims.QueryRequest{Scope: claims.Scope{Group: " Group "}}); err != nil {
		t.Fatal(err)
	}
	if got, want := calls.queryCalls[0].Environments, []claims.Environment{claims.Prod, claims.Sandbox}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty query environments = %#v, want %#v", got, want)
	}
	chatActor := claims.Actor{Email: "chat@example.com", Issuer: "https://chat.example", Subject: "users/1", Channel: claims.GoogleChat}
	if _, err := service.Query(context.Background(), chatActor, claims.QueryRequest{}); err != nil {
		t.Fatalf("Chat all-groups query was rejected: %v", err)
	}
	if calls.queryCalls[1].Scope.Group != "" || calls.queryCalls[1].Scope.App != "" {
		t.Fatalf("Chat list scope was rewritten: %#v", calls.queryCalls[1].Scope)
	}
	if _, err := service.Query(context.Background(), restActor(), claims.QueryRequest{}); err == nil {
		t.Fatal("REST query without a group unexpectedly succeeded")
	}
}

func TestServiceRejectsMalformedMutationInputs(t *testing.T) {
	store, _ := newRecordingStore(t)
	service := claims.NewService(store)
	valid := claims.AcquireRequest{Scope: claims.Scope{Group: "group"}, Environments: []claims.Environment{claims.Sandbox}, RequestID: "request-1"}
	invalidRequests := []claims.AcquireRequest{
		{Scope: valid.Scope, Environments: []claims.Environment{claims.Sandbox, " SANDBOX "}, RequestID: valid.RequestID},
		{Scope: valid.Scope, Environments: []claims.Environment{}, RequestID: valid.RequestID},
		{Scope: valid.Scope, Environments: []claims.Environment{"production"}, RequestID: valid.RequestID},
		{Scope: valid.Scope, Environments: []claims.Environment{claims.Sandbox}, RequestID: ""},
		{Scope: valid.Scope, Environments: []claims.Environment{claims.Sandbox}, RequestID: " "},
		{Scope: valid.Scope, Environments: []claims.Environment{claims.Sandbox}, RequestID: "request-é"},
		{Scope: valid.Scope, Environments: []claims.Environment{claims.Sandbox}, RequestID: strings.Repeat("x", 129)},
		{Scope: claims.Scope{Group: "bad_group"}, Environments: []claims.Environment{claims.Sandbox}, RequestID: valid.RequestID},
	}
	for _, request := range invalidRequests {
		if _, err := service.Acquire(context.Background(), restActor(), request); err == nil {
			t.Errorf("Acquire(%#v) unexpectedly succeeded", request)
		} else {
			requireDomainError(t, err, claims.Invalid)
		}
	}
	if _, err := service.Release(context.Background(), restActor(), claims.ReleaseRequest{ClaimID: "not-a-uuid", RequestID: "release-1"}); err == nil {
		t.Fatal("Release accepted a malformed claim ID")
	} else {
		requireDomainError(t, err, claims.Invalid)
	}
}

func TestNilExpectedRevisionIsChatOnly(t *testing.T) {
	store, calls := newRecordingStore(t)
	service := claims.NewService(store)
	request := claims.ExpiryRequest{
		ClaimID: "01234567-89ab-cdef-0123-456789abcdef", ExpiresAt: time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC), RequestID: "expiry-1",
	}
	if _, err := service.ChangeExpiry(context.Background(), restActor(), request); err == nil {
		t.Fatal("REST expiry change without expected revision unexpectedly succeeded")
	} else {
		requireDomainError(t, err, claims.Invalid)
	}
	chatActor := claims.Actor{Email: "chat@example.com", Issuer: "https://chat.example", Subject: "users/1", Channel: claims.GoogleChat}
	if _, err := service.ChangeExpiry(context.Background(), chatActor, request); err != nil {
		t.Fatalf("Chat expiry change without revision was rejected: %v", err)
	}
	if calls.expiryCalls[0].ExpectedRevision != nil {
		t.Fatalf("nil Chat expected revision was not preserved: %#v", calls.expiryCalls[0].ExpectedRevision)
	}
}
