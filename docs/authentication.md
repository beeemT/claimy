# Authentication configuration

Use this guide to configure every Claimy authentication path. Claimy verifies RS256 JWTs with the HTTPS JWKS URLs in its configuration. It never gets a key URL from a token.

The operator owns the public HTTPS route, certificate, ingress, and network policy. Claimy serves HTTP on its configured port.

## Configure the server

Claimy loads `config.dist.yml` from its working directory. Copy it to private deployment configuration. Gosoline also accepts upper-case environment overrides. Convert dots to underscores and use upper case:

```sh
SQLC_DEFAULT_URI_HOST=db.example.internal \
HTTPSERVER_DEFAULT_PORT=8088 \
CLAIMY_AUTH_TEAM_DOMAIN=example.com \
CLAIMY_AUTH_REST_AUDIENCE=claimy-manual \
claimy serve
```

Keep issuer URLs and audiences in configuration. Keep tokens, private keys, and passwords in a secret mechanism.

For Helm deployments, see the [Claimy chart guide](../build/helm/claimy/README.md). The chart `config` value uses the same shape as `config.dist.yml`.
Set `config.claimy.auth` and `config.claimy.chat` in that value. Do not add a second top-level authentication block.
The database password must come from an existing Kubernetes Secret. Set `database.existingSecret` and `database.passwordKey` in Helm values. Do not put the password in `values.yaml` or `config`.

Use this authentication configuration as a starting point:

```yaml
claimy:
  auth:
    team_domain: example.com
    rest:
      issuer: https://accounts.google.com
      audience: claimy-manual
      jwks_url: https://www.googleapis.com/oauth2/v3/certs
    gitlab:
      issuer: https://gitlab.example.com
      audience: https://claimy.example.com
      jwks_url: https://gitlab.example.com/oauth/discovery/keys
    chat:
      issuer: https://accounts.google.com
      audience: https://claimy.example.com/v1/chat/events
      jwks_url: https://www.googleapis.com/oauth2/v3/certs
    cli:
      enabled: false
      client_id: claimy-manual
      scopes: [openid, email]
      authorization_params:
        access_type: offline
        prompt: consent
  chat:
    app_identity: claimy
    allowed_spaces: [spaces/AAAA]
    deadline: 25s
```

`team_domain` is required. Claimy lower-cases it and checks that it is a DNS-style ASCII domain. Manual REST users, GitLab `user_email`, and Google Chat human users must use this domain.

Claimy validates all three issuer blocks at startup. This happens even when no caller uses one block. Each issuer and JWKS URL must be an absolute HTTPS URL without userinfo, query, or fragment. Each audience must be a non-empty exact string. The Chat issuer must be `https://accounts.google.com`. REST and GitLab issuers must differ. One issuer cannot use two JWKS URLs.

A bad authentication block stops startup. Startup also requires reachable MySQL and the complete supported schema. See the database section in the README.

`claimy.auth.cli` is optional and defaults to disabled. When it is disabled, `GET /v1/auth/config` returns `404`. Claimy does not require browser-login settings in this mode.

## JWT rules and audiences

REST requests accept a bearer token from the configured manual REST issuer or GitLab issuer. Claimy checks the exact issuer, exactly one configured audience, `exp`, optional `nbf` and `iat`, an RS256 signature, and a key from that issuer's configured JWKS URL. Claimy caches keys and refreshes them for rotation.

Claimy rejects unknown issuers, wrong audiences, bad time claims, unsupported algorithms, invalid keys, and expired tokens. A manual REST token must contain a stable `sub` and an `email`. Claimy checks that email against `team_domain`.

Claimy accepts stable IDs from 1 to 128 ASCII bytes. The first byte must be a letter or digit. Other bytes can be letters, digits, `.`, `_`, `:`, or `-`.
This rule applies to manual `sub`, GitLab user, project, and job IDs, and the ID in a Chat `users/<id>` name. A Chat resource name must be at most 128 ASCII bytes total.
For example, `auth0|123` is not a valid `sub`. Claimy rejects it with `401 Unauthorized`.

The transport URL is where the client sends an HTTP request, such as `https://claimy.example.com/v1/claims/acquire`. The JWT audience is the exact value in `aud`. Claimy compares it with `claimy.auth.*.audience`. These values are not derived from each other. Request and configure the exact audience string, including any path or trailing slash.

Claimy supports generic OIDC providers only when they meet this contract. It does not support opaque access tokens, HMAC-only tokens, or non-RS256 signing. Confirm issuer, JWKS, claims, and signing algorithm support with the provider.

## Manual REST and OIDC

Set `claimy.auth.rest` to the issuer that signs manual user ID tokens. Set its exact audience and HTTPS JWKS endpoint. The provider must issue RS256 ID tokens with `sub`, `email`, `iss`, `aud`, and valid time claims.

Register a client with the provider. Make the client issue an ID token with those claims. Set its audience to the configured REST audience. Claimy does not accept an arbitrary OAuth access token from the same provider.

## Optional browser CLI login

Browser login is disabled by default. It is separate from GitLab CI and Google Chat authentication.

Complete these steps to enable browser login:

1. Register a public OIDC client with the manual REST provider.
2. Do not create or configure a client secret. Claimy accepts public client metadata only.
3. Set `claimy.auth.cli.enabled: true` after the provider meets the requirements below.
4. Use the configured `claimy.auth.rest.issuer`. Claimy publishes it as the browser-login issuer.
5. Set `claimy.auth.cli.client_id` equal to `claimy.auth.rest.audience`.
6. Set `claimy.auth.cli.scopes` to unique values that include `openid` and `email`.
7. Add `offline_access` only when the provider requires it for a refresh token.
8. Add only non-secret provider options to `authorization_params`, such as `prompt` or `access_type`.
9. Register loopback HTTP redirects with the exact path `/callback`.

Claimy chooses an ephemeral loopback port. It listens only on `127.0.0.1:<port>/callback`. It uses Authorization Code flow, S256 PKCE, state, and nonce. The provider must advertise RS256. If it advertises PKCE methods, it must advertise S256.

Claimy supplies protected OAuth parameters. Do not set `state`, `nonce`, `redirect_uri`, `client_id`, `scope`, `response_type`, code, token, secret, or verifier parameters in `authorization_params`. Claimy allows at most 32 parameters.

The initial authorization-code response must contain an access token and a refresh token. It must also contain an ID token with `sub` and `email`.
The issuer must match the configured issuer. The ID token's only audience must equal the client ID. Claimy verifies it before calling `/v1/auth/me`.

`GET /v1/auth/config` is a public, secret-free endpoint. When enabled, it returns only the issuer, client ID, scopes, and allowed authorization parameters. It returns no client secret, access token, or refresh token. The CLI uses OIDC discovery for authorization, token, and JWKS endpoints. It requires valid TLS and secure provider endpoints.

Run the CLI login on a machine with a browser and an OS credential store:

```sh
claimy auth login https://claimy.example.com
claimy query --group payments --environments sandbox
claimy auth logout https://claimy.example.com
```

The CLI stores the refresh credential and its server and provider binding in the OS credential store. It stores only the default server URL in `claimy/config.json`. It does not store an access token in that file.

Each operation with a saved login requires an access token and a verifiable ID token from the refresh response. Claimy preserves the saved subject and email.
The CLI saves a rotated refresh token under a per-server lock. `logout` deletes local credentials and the matching default URL.
It does not call provider revocation.

The noninteractive credential order is:

1. `--token-file`.
2. `CLAIMY_ID_TOKEN`.
3. The saved browser login.

`--url` overrides `CLAIMY_URL` and the saved server URL. Put global flags before the command. The scratch runtime image has no browser or credential-store service, so use an environment token or token file there.

## GitLab CI

GitLab CI uses a GitLab ID token. It does not use browser login, the OS credential store, `CI_JOB_TOKEN`, or GitHub Actions OIDC.

Configure `claimy.auth.gitlab` for the GitLab instance that signs the token:

1. Set `issuer` to the exact token `iss`.
2. Set `jwks_url` to that issuer's HTTPS signing-key endpoint.
3. Set `audience` to the exact `aud` requested by the job.
4. Set `claimy.auth.team_domain` to the permitted user email domain.

Use an inherited or prepared job image that contains the Claimy CLI and a shell:

```yaml
claim:
  variables:
    CLAIMY_URL: https://claimy.example.com
    CLAIMY_GROUP: payments
  id_tokens:
    CLAIMY_ID_TOKEN:
      aud: https://claimy.example.com
  script:
    - claimy query --group "$CLAIMY_GROUP" --environments sandbox
```

Use `claimy acquire` before deployment. Pass `--environments sandbox` and a stable `--request-id`. Save the exact claim ID for cleanup. Run `claimy release` in `after_script` with the saved ID and a stable release request ID. GitLab starts `after_script` in a new shell, so put values needed there in job variables or files.

Proceed with deployment only when `acquire` exits with status `0`. Exit status `1` means busy or an inactive replay. Exit status `2` means invalid input, authentication, transport, storage, or response-shape failure.

Claimy requires `user_id`, `user_email`, `job_id`, and a project ID. `job_project_id` takes precedence when present. Claimy rejects a malformed present `job_project_id`; it does not fall back to `project_id`.

Claimy checks GitLab `iss`, `aud`, `exp`, `nbf`, and `iat`. It checks the user email against `team_domain`. It records `user_email` as the owner and retains stable user, project, and job IDs as CI identity.

A retry with a new GitLab job ID is a new token principal. It cannot replay the previous job request. Jobs from the same user can still overlap where the claim model permits same-email ownership. This is not a strict per-job mutex.

The CLI reads the token from `CLAIMY_ID_TOKEN`. `--token-file` overrides that variable. The variable overrides a saved login. Disable shell tracing. Never place a token in a command argument, image layer, log, or repository.

The shell helpers are an alternative. See [`scripts/gitlab-claim-example.yml`](../scripts/gitlab-claim-example.yml) and [`scripts/ci-acquire.sh`](../scripts/ci-acquire.sh).

## Google Chat

Configure the Chat app to send `MESSAGE` events to `POST /v1/chat/events` through the public HTTPS route.

Complete these steps in Google Chat:

1. Create or select the Google Cloud project for the Chat app.
2. Enable the Google Chat API.
3. Configure the Chat app as an HTTP endpoint.
4. Set the endpoint URL to the public Claimy event URL.
5. Select the HTTP endpoint URL as the audience option for request authentication.
6. In Google Chat API > Configuration > Commands, add `/claim` as a Slash command with a positive command ID.
7. Add the exact Chat space names to `claimy.chat.allowed_spaces`.
8. Set `claimy.chat.app_identity` to the configured app identity.
9. Set `claimy.chat.deadline` to a value from `1ns` through `30s`.

The repository includes a transparent 1254×1254 PNG [Google Chat avatar](google-chat-avatar.png) for copying and uploading. Upload it to a public HTTPS image host, then set its URL in Google Chat API > Configuration > Application info > Avatar URL and save. Claimy does not serve this file automatically. The app avatar is controlled by this Google Cloud configuration, not Claimy's message responses or browser callback favicon. See Google's [Chat API configuration guide](https://developers.google.com/workspace/chat/configure-chat-api).

Google requires a slash command ID from 1 to 1000. Claimy accepts only `MESSAGE` events with a positive `message.slashCommand.commandId`.
Claimy does not pin the command ID in configuration. It expects the command text to use `/claim`.

Claimy does not need a Google service-account private key. Claimy does not call the Chat API. Claimy therefore does not need the outgoing `chat.bot` scope. Google Chat sends an authenticated service-account ID token with each incoming event.

Claimy verifies the token with `claimy.auth.chat`. The token must use issuer `https://accounts.google.com`, RS256, valid times, and the configured exact audience. It must contain `email: chat@system.gserviceaccount.com` and `email_verified: true`.

The event user must have type `HUMAN`, a valid resource name, and a team-domain email. Claimy records this human as the owner. It never records the service account as the owner.

`allowed_spaces` must contain at least one unique valid `spaces/<id>` name. Claimy rejects events from other spaces. The Chat audience is the JWT `aud` value. It is not automatically the transport URL. The normal configuration uses the public event URL, including `/v1/chat/events`, as the exact audience.

Read Google's [Verify requests from Google Chat](https://developers.google.com/workspace/chat/verify-requests-from-chat) guide for current Chat app setup and audience options.

## Separation, limits, and troubleshooting

Use separate trust settings for manual REST, GitLab, and Chat. Do not reuse a private signing key across providers. JWKS endpoints publish public keys. They do not store secrets.

Treat bearer tokens, refresh credentials, database passwords, and provider private keys as secrets. Never put them in logs or support tickets.

Claims expire and remain advisory leases. They are not deployment fencing. Same-email claims can overlap independently. A different email can make acquisition busy.

A request ID replay returns the original mutation result. An inactive replay remains inactive. Use a new request ID for a deliberate new attempt. Token expiry, key rotation, refresh-token rotation, callback timeout, and local cleanup are expected failure cases.

| Status | Meaning |
| --- | --- |
| `401 Unauthorized` | Missing or invalid bearer token, signature, issuer, audience, time, identity claim, or Chat event user. |
| `403 Forbidden` | Valid identity outside `team_domain`, or valid Chat identity from a disallowed space. `/v1/auth/me` also rejects CI identities. |
| `404 Not Found` | Browser login is disabled at `/v1/auth/config`. |
| `400 Bad Request` | Invalid JSON, Chat event, Chat space, command, or claim input. |
| `500 Internal Server Error` | REST reports a sanitized internal error when JWKS retrieval fails. Other internal or database errors can also return `500`. |
| `503 Service Unavailable` | Chat returns this status for JWKS retrieval and temporary dependency failures. Check outbound HTTPS, DNS, CA trust, and the configured JWKS endpoint. |
| `200` with `acquired: false` | The authenticated request is busy. This is not an authentication failure. |

Use redacted configuration paths, issuer names, status codes, and provider health in troubleshooting. Do not decode or print live tokens, refresh credentials, authorization codes, or private keys.

## Sources

Repository sources: [`config.dist.yml`](../config.dist.yml), `internal/auth/auth.go`, `internal/auth/types.go`, `internal/auth/jwks.go`, `internal/application/runtime.go`, `internal/api/api.go`, `internal/chat/chat.go`, `internal/login/login.go`, `internal/login/oidc.go`, `internal/login/callback.go`, `internal/login/store.go`, `scripts/ci-acquire.sh`, and [`scripts/gitlab-claim-example.yml`](../scripts/gitlab-claim-example.yml).

Provider references: [GitLab OIDC ID tokens](https://docs.gitlab.com/ci/secrets/id_token_authentication/), [GitLab CI `id_tokens`](https://docs.gitlab.com/ci/yaml/#id_tokens), [Google Chat request verification](https://developers.google.com/workspace/chat/verify-requests-from-chat), [Google Chat slash commands](https://developers.google.com/workspace/chat/commands), and [OpenID Connect Core](https://openid.net/specs/openid-connect-core-1_0.html#AuthorizationEndpoint).
