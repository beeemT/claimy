package auth

import (
	"context"

	"github.com/beeemT/claimy/internal/claims"
)

// User is the authenticated human user from a Chat event.
type User struct {
	Name  string
	Email string
	Type  string
}

// IssuerSettings defines an allowed token issuer and its key service.
type IssuerSettings struct {
	Issuer   string `cfg:"issuer"`
	Audience string `cfg:"audience"`
	JWKSURL  string `cfg:"jwks_url"`
}

// CLISettings configures the optional browser-login metadata exposed by Claimy.
// It contains only public OAuth client metadata; no client secret is accepted.
type CLISettings struct {
	Enabled             bool              `cfg:"enabled" default:"false"`
	ClientID            string            `cfg:"client_id"`
	Scopes              []string          `cfg:"scopes"`
	AuthorizationParams map[string]string `cfg:"authorization_params"`
}

// Settings contains trusted identity providers, the Claimy team domain, and
// optional browser-login metadata.
type Settings struct {
	TeamDomain string         `cfg:"team_domain"`
	REST       IssuerSettings `cfg:"rest"`
	GitLab     IssuerSettings `cfg:"gitlab"`
	Chat       IssuerSettings `cfg:"chat"`
	CLI        CLISettings    `cfg:"cli"`
}

// Authenticator verifies signed tokens and establishes a team-member actor.
type Authenticator interface {
	REST(context.Context, string) (claims.Actor, error)
	Chat(context.Context, string, User) (claims.Actor, error)
}

// KeyProvider obtains a public verification key from a configured trusted URL.
type KeyProvider interface {
	Key(context.Context, string, string) (any, error)
}
