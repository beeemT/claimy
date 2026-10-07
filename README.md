# Claimy

Claimy coordinates manual, GitLab CI, and Google Chat claims for an app or its whole group in `sandbox`, `prod`, or both.
See [the environment-claim plan](docs/environment-claim-plan.md) for the complete product contract and acceptance scenarios.

Claims start at database operation time and always expire.
The default expiry is noon on the next calendar day in `Europe/Berlin`.
Same-email claims may overlap independently. Another owner's overlapping claim makes acquisition busy.
A group claim also covers apps added later.

Any authenticated team member may release or change any manual or CI claim.
The service records the actor and keeps the owner and scope unchanged.
Claims are advisory leases, not deployment fencing or production approvals.

## Development

Install [Mise](https://mise.jdx.dev/) and Docker. Run these commands from the repository root:

```sh
mise trust
mise install
mise run setup
mise run check
```

The setup task downloads public Go modules and installs Prek Git hooks.
An ordinary clone and a Git worktree use the same setup.
The `worktree-setup` task also runs setup for worktree tools.
Integration tests start disposable MySQL 8.0.42 containers and apply the real Goose migrations.
A separate lifecycle test verifies migration reversal and restoration.
They never use a shared database.

## Database and configuration

Claimy loads `config.dist.yml` from its working directory and listens on port 8088.
The committed distribution and test files contain example identities and development-only values.
Replace them before deployment. Do not commit local configuration or credentials.

Use a dedicated MySQL database with InnoDB and enforced `CHECK` constraints.
The tested baseline is MySQL 8.0.42. Claimy requires at least 8.0.16.
Set `claimy.mysql_version` to the exact supported server version.
The SQLC connection must use `loc: UTC` and `time_zone: "'+00:00'"`.
Runtime startup rejects an unexpected server version, non-UTC session, or incomplete InnoDB schema.

Apply migrations explicitly to the selected database:

```sh
# Supply a dedicated database DSN through your local secret mechanism.
export GOOSE_DBSTRING
mise run migrate
mise run run
```

The migration task requires `GOOSE_DBSTRING` and selects the MySQL driver.
Runtime migrations are disabled by default.
The service prunes retained data daily and closes its database after HTTP shutdown completes.

Use environment variables to override configuration, for example:

```sh
SQLC_DEFAULT_URI_HOST=127.0.0.1 HTTPSERVER_DEFAULT_PORT=9090 mise run run
```

Configure these identity settings before normal use:

- `claimy.auth.team_domain`: the allowed account email domain.
- `claimy.auth.rest`: the trusted Google-style issuer, exact audience, and HTTPS JWKS URL.
- `claimy.auth.gitlab`: the trusted GitLab issuer, exact audience, and HTTPS JWKS URL.
- `claimy.auth.chat`: the Google issuer, endpoint-URL audience, and HTTPS JWKS URL.
- `claimy.chat.app_identity`, `allowed_spaces`, and `deadline`: the Chat app, allowed test/team spaces, and bounded event deadline.

JWT verification uses RS256, configured key URLs, normal TLS validation, exact issuer/audience, and token times.
GitLab ownership uses `user_email` and stable user, job, and project IDs.
`job_project_id` takes precedence when present. A malformed present value is rejected.
Chat verifies `chat@system.gserviceaccount.com`, then derives ownership from the event's authenticated human user.
The service account is never the claim owner.

Expose the service through an HTTPS ingress.
Do not log bearer tokens. Keep existing deployment approvals and database backups.

## REST API

[api/openapi.yaml](api/openapi.yaml) is the generated public API specification.
Its source is `api/openapi.yaml.gotempl` and the YAML fragments under `api/`.
The generated models and HTTP transport are committed in `pkg/client/client.gen.go`.
API calls require a bearer token, except the public browser-login configuration endpoint.
Every mutation requires a stable `requestId`.

| Method | Path | Behavior |
|---|---|---|
| POST | `/v1/claims/acquire` | Atomically acquire the selected environments or return busy |
| POST | `/v1/claims/query` | Read current, retained historical, or projected future state |
| POST | `/v1/claims/{id}/release` | Release one claim or report it inactive |
| PATCH | `/v1/claims/{id}` | Change expiry with `expectedRevision` |
| POST | `/v1/catalog/groups/query` | List registered groups |
| GET | `/v1/catalog/groups/{group}` | Read a registered group |
| POST | `/v1/catalog/groups/{group}/apps/query` | List that group's registered apps |
| GET | `/v1/auth/config` | Read public browser-login metadata; return 404 when login is disabled |
| GET | `/v1/auth/me` | Confirm an authenticated manual REST identity; reject CI identities |

Omit `app` to select a whole group. A supplied empty or null app is invalid.
Use one or both values from `sandbox` and `prod` in `environments`.
Custom REST times require RFC3339 with an offset.
Catalog lists accept `{filter:{canonicalName:"api"},page:{limit:20,offset:0}}`.
Their default and maximum page size is 100.
Catalog reads never register or mutate resources.

Busy acquisition returns HTTP 200 with `acquired:false`.
Validation, authentication, forbidden membership, missing catalog items, old history, and revision/request mismatches return 400, 401, 403, 404, 410, and 409.
Storage or identity-service failures return 5xx, never busy.
An acquisition replay returns its original claim and expiry with current `activeNow`.
A busy replay remains busy. Use a new request ID for a deliberate new attempt.

The framework health endpoint is `GET /health`:

```sh
curl --fail http://localhost:8088/health
```

A healthy process returns HTTP 200 and `{}`.
The binary includes Berlin timezone data and handles SIGINT and SIGTERM shutdown.

## API client and CLI

The public Go client is `github.com/beeemT/claimy/pkg/client`.
It returns typed responses and structured `*client.APIError` values.
Its default HTTP timeout is 30 seconds. It does not retry mutations.

```go
api, err := client.New(serverURL, client.WithBearerToken(idToken))
if err != nil {
    return err
}
result, err := api.Acquire(ctx, client.AcquireRequest{
    Group:        "payments",
    Environments: []client.Environment{client.Sandbox},
    RequestId:    "deploy-123",
})
```

Client commands run directly under `claimy`. Use `claimy serve` to start the server.
Bare `claimy` and unknown commands do not start the server.
Use browser login below, or set `CLAIMY_URL` and `CLAIMY_ID_TOKEN` for noninteractive calls.
Explicit `--token-file` overrides the environment token; the environment token overrides a saved login.
`--url` and `CLAIMY_URL` override the saved default server.
Global flags must precede the command. Do not pass tokens as command-line arguments or enable shell tracing.
`--timeout` sets a positive API timeout, such as `10s`, including credential refresh.

```sh
claimy --url https://claimy.example.com --token-file ./id-token acquire \
  --group payments --app gateway --environments sandbox \
  --request-id deploy-123 --output id
claimy --json query --group payments --environments both
claimy expiry --id "$CLAIM_ID" --expires-at 2026-12-31T12:00:00+01:00 \
  --revision 1 --request-id expiry-123
claimy release --id "$CLAIM_ID" --request-id release-123
claimy catalog groups --limit 20 --offset 0
claimy catalog group --group payments
claimy catalog apps --group payments --canonical-name gateway
```

Acquisition requires an explicit stable request ID. Supported environment selections are
`sandbox`, `prod`, `both`, and `sandbox,prod`.
JSON is the default output. `--json` also selects it explicitly, before or after the operation.
`acquire --output id` emits only the ID of an acquired, active claim; it cannot be combined with `--json`.
Acquisition exits with 0 for acquired and active, 1 for busy or an inactive replay, and 2 for an error.
Other commands exit with 0 for success and 2 for an error.
Errors go to standard error. Busy is not a storage, authentication, or transport failure.

The same CLI is in the Docker image:

```sh
docker run --rm -e CLAIMY_URL -e CLAIMY_ID_TOKEN beeemt/claimy query \
  --group payments --environments both
```

The image defaults to `serve`; an explicit client command replaces that default.

### Browser login and keychain

```sh
claimy auth login https://claimy.example.com
claimy query --group payments --json
claimy auth logout
# Select another saved server explicitly:
claimy auth logout https://claimy.example.com
```

Login opens the configured identity provider in a browser with Authorization Code + S256 PKCE.
The callback uses a temporary loopback listener and has a five-minute deadline.
If automatic browser opening fails, open the URL printed to standard error.
The CLI verifies the signed ID token and confirms team access with Claimy before it saves a login.

Only the refresh credential and its server/identity binding are stored in the OS credential store:
macOS Keychain, Linux Secret Service, or Windows Credential Manager.
The regular `claimy/config.json` file under the OS user configuration directory stores only the default server URL.
Each authenticated command refreshes the ID token; rotating refresh credentials are saved under a per-server lock.
Logout removes the local credential and matching default URL. It does not revoke the provider session.
Secure-storage failures stop login or refresh. There is no plaintext fallback.
The scratch Docker image has no browser or credential-store service; use an environment token or token file there.

Browser login is disabled in the distribution configuration. To enable it:

1. Register an OIDC **public client** that supports S256 PKCE and a loopback callback at
   `http://127.0.0.1:<dynamic-port>/callback`. It must issue refresh credentials and ID tokens on refresh.
2. Set the manual REST issuer and JWKS URL, and set `claimy.auth.rest.audience` to this client's ID.
3. Set `claimy.auth.cli.enabled: true` and `claimy.auth.cli.client_id` to the same ID.
4. Include `openid` and `email` in `claimy.auth.cli.scopes`. Add `offline_access` if the provider requires it.
   Set only supported, non-secret consent options in `authorization_params`, such as `prompt`.

Providers that require a client secret are not supported by this public-client flow.
The CLI obtains these public settings from the Claimy URL; users do not need to enter the issuer or client ID.
Production server and issuer URLs must use HTTPS. Existing GitLab and Chat authentication is unchanged.
The native browser, macOS Keychain, and refresh path were exercised with a local signed OIDC fixture;
a deployed provider registration remains an operator prerequisite.

## GitLab CI caller

Use [scripts/ci-acquire.sh](scripts/ci-acquire.sh) and [scripts/ci-release.sh](scripts/ci-release.sh).
[scripts/gitlab-claim-example.yml](scripts/gitlab-claim-example.yml) shows ID-token and completion-cleanup configuration.
Set `CLAIMY_URL`, `CLAIMY_ID_TOKEN`, `CLAIMY_GROUP`, `CLAIMY_REQUEST_ID`, and `CLAIMY_ENVIRONMENTS`.
Set optional `CLAIMY_APP` and `CLAIMY_EXPIRES_AT` only when needed.

The acquire script makes one request and returns these exit codes:

- 0: acquired and active. Standard output contains the claim ID.
- 1: busy or an inactive successful replay. Do not deploy.
- 2: invalid input, authentication, transport, storage, or response-shape failure. Do not deploy.

Preserve the claim ID for best-effort `after_script` release.
Release failure must not replace the deployment result. Expiry handles interrupted jobs.
Never enable shell tracing around bearer tokens.

## Google Chat

Configure a Chat app to send authenticated slash-command `MESSAGE` events to `POST /v1/chat/events`.
Use the public endpoint URL as the token audience and configure the allowed space names.
Events must include a human user name and team email.

```text
/claim take <sandbox|prod|both> <group> [app <app>] [until <Berlin time>]
/claim free <sandbox|prod|both> <group> [app <app>] [at <time>]
/claim list [<sandbox|prod>] [at <time>]
/claim release <claim-id>
/claim expiry <claim-id> until <Berlin time>
```

`free` queries availability. It does not release a claim.
Replies show `free` separately from `allowedForCaller` and mark future results as projections.
Ambiguous or nonexistent Berlin local times require an explicit offset.
The app rejects unstructured commands and never infers intent with NLP.
Duplicate message delivery returns the saved mutation result.
Changed intent on the same message returns a conflict.

## Checks and container smoke

Mise pins Go, gofumpt, golangci-lint, Prek, Gitleaks, Mockery, gotempl, oapi-codegen, and Goose.
Run `mise run generate` after generator input changes.
`generate-check` rejects tracked and untracked generated drift.
`version-check` compares Go, template/code generators, Mockery, and Goose module/tool versions.
CodeRabbit review settings and path-specific contract guidance are in [.coderabbit.yaml](.coderabbit.yaml).
Automatic incremental reviews apply to non-draft pull requests; review settings do not change merge requirements.

```sh
mise run check
mise exec -- prek run --all-files
docker build --tag claimy:ci .
CLAIMY_IMAGE=claimy:ci mise exec -- go test -count=1 -tags=integration,fixtures ./test/e2e
```

The image smoke starts the actual container against disposable MySQL and an HTTPS JWKS fixture with a test CA.
It exercises the public client, native and container CLI, signed REST and Chat requests, persisted ownership/history, the health endpoint, storage-failure exit codes, and graceful shutdown.
Without `CLAIMY_IMAGE`, normal integration tests skip only the image-specific test.
The runtime image is static, nonroot, and contains the safe configuration, migrations, CA bundle, and embedded timezone data.
It contains no shell or build tools.

## Continuous integration and publishing

GitHub Actions runs repository checks and the database-backed image smoke.
Public actions are pinned to commit hashes. Repository permissions are read-only.
Pull requests do not use Docker Hub credentials or publish images.
After checks pass, `main` and valid Git-tag pushes publish to `docker.io/beeemt/claimy`.
The full commit hash and `latest` identify one image digest.
Tag pushes also publish the exact Git tag. OCI labels retain the commit revision and release version.
The workflow does not deploy the service.

Configure `DOCKERHUB_TOKEN` with Docker Hub **Read & Write** permissions, not Delete access.
The username uses `DOCKERHUB_USERNAME` from an Actions secret or variable, with `beeemt` as the default.
Never put registry credentials in source, build arguments, image layers, or pull-request fixtures.

## License

[MIT](LICENSE).
