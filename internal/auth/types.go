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

// Settings contains trusted identity providers and the Claimy team domain.
type Settings struct {
	TeamDomain string         `cfg:"team_domain"`
	REST       IssuerSettings `cfg:"rest"`
	GitLab     IssuerSettings `cfg:"gitlab"`
	Chat       IssuerSettings `cfg:"chat"`
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
