package login

import (
	"testing"

	"github.com/beeemT/claimy/pkg/client"
)

func TestValidateLoginConfigRejectsCredentialAndOversizedParameters(t *testing.T) {
	cases := []client.LoginConfig{
		{Issuer: "http://127.0.0.1:1", ClientId: "cli", Scopes: []string{"openid", "email"}, AuthorizationParams: mapPointer(map[string]string{"client_secret": "not-allowed"})},
		{Issuer: "http://127.0.0.1:1", ClientId: "cli", Scopes: []string{"openid", "email"}, AuthorizationParams: mapPointer(map[string]string{"x": ""})},
	}
	for _, config := range cases {
		if err := validateLoginConfig(&config); err == nil {
			t.Fatal("disallowed authorization metadata was accepted")
		}
	}
	params := make(map[string]string)
	for index := 0; index < 33; index++ {
		params[string(rune('a'+index%26))+string(rune('0'+index/26))] = "value"
	}
	config := client.LoginConfig{Issuer: "http://127.0.0.1:1", ClientId: "cli", Scopes: []string{"openid", "email"}, AuthorizationParams: mapPointer(params)}
	if err := validateLoginConfig(&config); err == nil {
		t.Fatal("too many authorization parameters were accepted")
	}
}

func mapPointer(value map[string]string) *map[string]string { return &value }
