package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/pkg/client"
	"github.com/gin-gonic/gin"
)

type loginAuthenticator struct {
	actor claims.Actor
}

func (a loginAuthenticator) REST(context.Context, string) (claims.Actor, error) {
	return a.actor, nil
}

func (loginAuthenticator) Chat(context.Context, string, auth.User) (claims.Actor, error) {
	return claims.Actor{}, claims.NewError(claims.Unauthenticated, "not supported in login handler test")
}

func loginContext(method string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, "/v1/auth/config", nil)

	return context, recorder
}

func TestLoginConfigDisabledReturnsStructuredNotFound(t *testing.T) {
	context, recorder := loginContext(http.MethodGet)
	service := &service{}
	service.loginConfig(context)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled login status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	var response client.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode disabled login response: %v", err)
	}
	if response.Error.Code != client.ErrorDetailCodeLoginDisabled || response.Error.Status != http.StatusNotFound {
		t.Fatalf("disabled login response = %#v", response)
	}
}

func TestLoginIdentityRequiresManualRESTActor(t *testing.T) {
	cases := []struct {
		name       string
		actor      claims.Actor
		wantStatus int
	}{
		{name: "manual", actor: claims.Actor{Channel: claims.REST, Email: "person@example.com"}, wantStatus: http.StatusOK},
		{name: "CI", actor: claims.Actor{Channel: claims.GitLabCI, Email: "job@example.com"}, wantStatus: http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, recorder := loginContext(http.MethodGet)
			context.Request = httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
			service := &service{identities: loginAuthenticator{actor: test.actor}}
			context.Request.Header.Set("Authorization", "Bearer fixture-token")
			service.loginIdentity(context)
			if recorder.Code != test.wantStatus {
				t.Fatalf("identity status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}
