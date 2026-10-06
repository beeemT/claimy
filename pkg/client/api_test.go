package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testAPI(t *testing.T, handler http.HandlerFunc) *API {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, err := New(server.URL)
	if err != nil {
		t.Fatalf("new API client: %v", err)
	}

	return api
}

func writeResponse(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()
	if _, err := w.Write(body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestAcquireRejectsMissingDiscriminator(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, []byte(`{}`))
	})

	_, err := api.Acquire(context.Background(), AcquireRequest{Group: "payments", Environments: []Environment{Sandbox}, RequestId: "missing-acquired"})
	if err == nil || !strings.Contains(err.Error(), "acquired") {
		t.Fatalf("missing acquired field error = %v", err)
	}
}

func TestAcquireRejectsMissingActiveNow(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, []byte(`{"acquired":true,"claim":{"id":"11111111-1111-1111-1111-111111111111","scope":{"group":"payments"},"environments":["sandbox"],"ownerEmail":"owner@example.test","source":"manual","createdAt":"2026-10-06T12:00:00Z","expiresAt":"2026-10-06T13:00:00Z","revision":1,"inherited":false}}`))
	})

	_, err := api.Acquire(context.Background(), AcquireRequest{Group: "payments", Environments: []Environment{Sandbox}, RequestId: "missing-active-now"})
	if err == nil || !strings.Contains(err.Error(), "activeNow") {
		t.Fatalf("missing activeNow error = %v", err)
	}
}

func TestAcquireAcceptsExplicitInactiveReplay(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, []byte(`{"acquired":true,"claim":{"id":"11111111-1111-1111-1111-111111111111","scope":{"group":"payments"},"environments":["sandbox"],"ownerEmail":"owner@example.test","source":"manual","createdAt":"2026-10-06T12:00:00Z","expiresAt":"2026-10-06T13:00:00Z","revision":1,"activeNow":false,"inherited":false}}`))
	})

	response, err := api.Acquire(context.Background(), AcquireRequest{Group: "payments", Environments: []Environment{Sandbox}, RequestId: "inactive-replay"})
	if err != nil {
		t.Fatalf("inactive replay: %v", err)
	}
	if response.Claim == nil || response.Claim.ActiveNow {
		t.Fatalf("inactive replay response = %#v", response)
	}
}

func TestAcquireRejectsBusyWithoutConflicts(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, []byte(`{"acquired":false}`))
	})

	_, err := api.Acquire(context.Background(), AcquireRequest{Group: "payments", Environments: []Environment{Sandbox}, RequestId: "missing-conflicts"})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("missing conflicts error = %v", err)
	}
}

func TestAPIErrorPreservesStructuredTaxonomy(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeResponse(t, w, []byte(`{"error":{"code":"unauthenticated","message":"authentication failed","status":401}}`))
	})

	_, err := api.Query(context.Background(), QueryRequest{Group: "payments"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("query error = %v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != ErrorDetailCodeUnauthenticated || apiErr.Message != "authentication failed" {
		t.Fatalf("APIError = %#v", apiErr)
	}
}

func TestAPIErrorFallbackDoesNotEchoBody(t *testing.T) {
	secret := "secret-response-body"
	api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeResponse(t, w, []byte(secret))
	})

	_, err := api.Query(context.Background(), QueryRequest{Group: "payments"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("query error = %v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable || strings.Contains(apiErr.Error(), secret) {
		t.Fatalf("fallback APIError = %#v", apiErr)
	}
}

func TestBearerTokenOptionRejectsUnsafeTokens(t *testing.T) {
	for _, token := range []string{"", "   ", "token\r\nvalue"} {
		t.Run(token, func(t *testing.T) {
			_, err := New("http://127.0.0.1", WithBearerToken(token))
			if err == nil || token != "" && strings.Contains(err.Error(), token) {
				t.Fatalf("token validation error = %v", err)
			}
		})
	}
}

func TestRequestHonorsContextCancellation(t *testing.T) {
	api := testAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := api.Query(ctx, QueryRequest{Group: "payments"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query error = %v, want context.Canceled", err)
	}
}

func TestCatalogRejectsNullSuccess(t *testing.T) {
	cases := []struct {
		name string
		call func(*API) error
	}{
		{
			name: "groups",
			call: func(api *API) error {
				_, err := api.ListGroups(t.Context(), CatalogListRequest{})

				return err
			},
		},
		{
			name: "group",
			call: func(api *API) error {
				_, err := api.GetGroup(t.Context(), "payments")

				return err
			},
		},
		{
			name: "apps",
			call: func(api *API) error {
				_, err := api.ListApps(t.Context(), "payments", CatalogListRequest{})

				return err
			},
		},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			api := testAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeResponse(t, w, []byte(" \nnull\t"))
			})
			if err := scenario.call(api); err == nil {
				t.Fatal("catalog accepted null as a successful response")
			}
		})
	}
}
