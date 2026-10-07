# Claimy

Claimy coordinates manual, GitLab CI, and Google Chat claims for an app or its whole group in `sandbox`, `prod`, or both.
Claims start at database operation time and always expire.
They are advisory leases.
They are not deployment fencing or production approvals.
See [the environment-claim plan](docs/environment-claim-plan.md) for the complete product contract and acceptance scenarios.

A claim has a group and, optionally, an app.
A group claim also covers apps added later.
Same-email claims may overlap independently.
An overlapping claim owned by another email makes acquisition busy.
Any authenticated team member may release or change a manual or CI claim.
The service records the actor.
It keeps the owner and scope unchanged.

## Contents

- [Install the released CLI](#install-the-released-cli)
- [Quick start](#quick-start)
- [Agent skill](#agent-skill)
- [CLI reference](#cli-reference)
- [Browser login and keychain](#browser-login-and-keychain)
- [Authentication configuration](docs/authentication.md)
- [GitLab CI caller](#gitlab-ci-caller)
- [REST API](#rest-api)
- [API client](#api-client)
- [Google Chat](#google-chat)
- [Development](#development)
- [Database and configuration](#database-and-configuration)
- [Helm installation and configuration](build/helm/claimy/README.md)
- [Checks and container smoke](#checks-and-container-smoke)
- [Continuous integration and publishing](#continuous-integration-and-publishing)
- [License](#license)

## Install the released CLI

The first stable Claimy release must be published before the Homebrew command below can work.
Do not reuse an older tag that predates this CLI.
See [Maintainer release prerequisites](#maintainer-release-prerequisites).

```sh
brew install beeemT/tap/claimy
```

The executable is `claimy`.
The packaged agent skill is at:

```text
$(brew --prefix claimy)/share/claimy/SKILL.md
```

The formula targets four native archives.
They are `darwin/arm64`, `darwin/amd64`, `linux/arm64`, and `linux/amd64`.
Each archive contains exactly `claimy`, `SKILL.md`, and `LICENSE` at its root.
The formula installs the executable and the skill.
It does not configure an agent for you.

## Quick start

The commands below use a released CLI and an HTTPS Claimy server.
Replace the URL, group, and app with values for your environment.

### Authenticate as a user

Browser login is an explicit user setup step.
Run it on a machine with a browser and an OS credential store:

```sh
claimy auth login https://claimy.example.com
claimy query --group payments --app gateway --environments sandbox
```

The login flow opens the configured identity provider.
It stores only the refresh credential and its server and identity binding in the OS credential store.
It does not print a bearer token.
For CI or another noninteractive context, set `CLAIMY_URL` and `CLAIMY_ID_TOKEN` through an existing credential mechanism.
You can also use `--token-file`.
Never pass a token as a command-line argument.

### Inspect, acquire, and release a claim

Query is only a view.
It does not reserve an environment.
Acquisition is the atomic ownership step.
It requires an environment selection and a stable request ID.

Run the following as one shell block or subshell.
Generate each request ID once with `uuidgen`.
Retain the request IDs for retries.
Retain the exact claim ID for cleanup.

```sh
claimy query --group payments --app gateway --environments sandbox

acquire_request_id="$(uuidgen)"
if claim_id="$(claimy acquire \
  --group payments \
  --app gateway \
  --environments sandbox \
  --request-id "$acquire_request_id" \
  --output id)"; then
  :
else
  acquire_status=$?
  if [ "$acquire_status" -eq 1 ]; then
    printf '%s\n' "Claim is busy or its replay is inactive" >&2
    exit 1
  fi
  printf '%s\n' "Claim acquisition failed" >&2
  exit 2
fi

release_request_id="$(uuidgen)"
cleanup() {
  claimy release --id "$claim_id" --request-id "$release_request_id" || \
    printf '%s\n' "Claim release failed. The finite expiry remains in effect." >&2
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Perform the authorized work here.
```

A retry of the same logical acquire uses the same `acquire_request_id`.
A deliberate new attempt uses a new request ID.
A retry of release uses the same `release_request_id`.
Do not run release with an empty claim ID.
Agents use separate tool shells in many environments.
Keep these values in agent context or caller-owned state when a later shell performs cleanup.
An interrupt can still prevent cleanup.
The finite expiry is the fallback.

Claims always expire.
Without `--expires-at`, the default is noon on the next calendar day in `Europe/Berlin`.
A custom expiry must be RFC3339 with an explicit offset.
For example, use `2026-12-31T12:00:00+01:00`.

### Use a noninteractive token

```sh
export CLAIMY_URL=https://claimy.example.com
: "${CLAIMY_ID_TOKEN:?Set CLAIMY_ID_TOKEN through an existing credential environment}"
export CLAIMY_ID_TOKEN
claimy query --group payments --environments both
```

Do not enable shell tracing while a token is present.
Do not commit tokens.
Do not put tokens in source, image layers, or command history.

## Agent skill

[skills/claimy/SKILL.md](skills/claimy/SKILL.md) is a small, user-installed Agent Skill.
It describes safe query, atomic acquire, and cleanup behavior.
It covers finite expiry, stable mutation request IDs, exact claim IDs, and exit `0`/`1`/`2` distinctions.
Claims remain advisory.
The skill does not replace deployment approvals or other authorization controls.

Install the packaged local file into the folder used by the agent.
The command below refuses to overwrite an existing destination.

```sh
skill_source="$(brew --prefix claimy)/share/claimy/SKILL.md"
skill_destination="$HOME/.agents/skills/claimy/SKILL.md"
if [ ! -f "$skill_source" ]; then
  printf '%s\n' "Claimy skill not found. Install the released claimy formula first." >&2
  exit 1
fi
if [ -e "$skill_destination" ] || [ -L "$skill_destination" ]; then
  printf '%s\n' "Refusing to overwrite existing skill: $skill_destination" >&2
  exit 1
fi
mkdir -p "$(dirname "$skill_destination")"
install -m 0644 "$skill_source" "$skill_destination"
```

The `~/.agents/skills` path is the personal skills location documented for Codex ([skills documentation](https://developers.openai.com/codex/skills)).
Claude Code uses `~/.claude/skills` for personal skills and `.claude/skills` for project skills ([skills documentation](https://code.claude.com/docs/en/skills)).
Copy the file to the appropriate folder for another agent.
Check that agent's documentation.
Agents do not all discover the same folder.
A project-local copy can be useful for a repository.
This installation is always explicit.
It does not modify agent configuration.
It does not execute the skill automatically.

When supported, invoke the skill with `$claimy` in Codex or `/claimy` in Claude Code.
You can also ask the agent to coordinate an authorized Claimy environment claim.
Do not assume that another agent supports these invocation forms.

The skill's browser-login instruction is for the user.
An agent must use an existing login or CI token.
It must not read, print, or extract tokens or keychain entries.

## CLI reference

Client commands run directly under `claimy`.
Use `claimy serve` only to start the HTTP service.
Bare `claimy` and unknown commands do not start the service.
Global flags must precede the command:

```text
claimy [--url URL] [--token-file PATH] [--timeout DURATION] [--json] <command>
```

- `--url` and `CLAIMY_URL` override the saved default server URL.
- `--token-file` reads a bearer token.
  It overrides `CLAIMY_ID_TOKEN` and a saved login.
- `CLAIMY_ID_TOKEN` overrides a saved login when `--token-file` is not supplied.
- `--timeout` must be positive, such as `10s`.
  It includes credential refresh.
- JSON is the default output.
  `--json` may be supplied globally or on an operation where accepted.

### Claim commands

```text
claimy acquire --group GROUP --environments ENVIRONMENTS --request-id ID [--app APP] [--expires-at RFC3339] [--output json|id]
claimy query --group GROUP [--app APP] [--environments ENVIRONMENTS] [--at RFC3339]
claimy release --id UUID --request-id ID
claimy expiry --id UUID --expires-at RFC3339 --revision REVISION --request-id ID
```

`ENVIRONMENTS` is `sandbox`, `prod`, `both`, or a comma-separated `sandbox,prod`.
`acquire` requires `--group`, `--environments`, and `--request-id`.
`query` requires `--group` and defaults to `both`.
Omit `--app` to select the whole group.
An explicitly empty app is invalid.

`acquire --output id` emits only the exact UUID of an acquired, active claim.
It cannot be combined with `--json`.
The normal JSON acquire response contains `acquired`.
It contains either an acquired `claim` or busy `conflicts`.
A claim includes `id`, `scope`, `environments`, `ownerEmail`, `source`, `createdAt`, `expiresAt`, `revision`, and `activeNow`.
It can also include release and inheritance details.
Each conflict includes the blocking claim `id`, `scope`, `environments`, `ownerEmail`, `source`, and `expiresAt`.

The query response contains `known`, `free`, `allowedForCaller`, `projected`, `at`, and `claims`.
A historical or future query does not acquire a claim.
The release and expiry responses contain `changed` and the resulting `claim`.
Expiry also requires the current expected `--revision`.

Exit statuses are part of the CLI contract:

- `acquire`: `0` means acquired and active.
  `1` means busy or an inactive successful replay.
  `2` means invalid input, authentication, transport, storage, or response-shape failure.
- Other client commands: `0` means success.
  `2` means an error.
- Errors go to standard error.
  Busy is not a storage, authentication, or transport failure.

A replay with the same acquire request ID returns the original claim and expiry with current `activeNow`.
A busy replay remains busy.
Use a new request ID for a deliberate new attempt.
Do not clear or extend another owner's claim without explicit approval.

### Catalog commands

Catalog reads never register or mutate resources.

```text
claimy catalog groups [--canonical-name NAME] [--limit 1..100] [--offset OFFSET]
claimy catalog group --group GROUP
claimy catalog apps --group GROUP [--canonical-name NAME] [--limit 1..100] [--offset OFFSET]
```

List responses contain `results` and `total`.
A group has `id`, `canonicalName`, and `createdAt`.
An app also has `groupId`.
The default and maximum page size is 100.

### Docker image

The same CLI is in the Docker image:

```sh
docker run --rm \
  -e CLAIMY_URL \
  -e CLAIMY_ID_TOKEN \
  beeemt/claimy query --group payments --environments both
```

The image defaults to `serve`.
An explicit client command replaces that default.
The scratch image has no browser or credential-store service.
Use an environment token or token file there.

## Browser login and keychain

```sh
claimy auth login https://claimy.example.com
claimy query --group payments --json
claimy auth logout
# Select another saved server explicitly:
claimy auth logout https://claimy.example.com
```

Login uses Authorization Code + S256 PKCE.
The callback uses a temporary loopback listener.
It sends the browser response before closing the listener.
Idle browser connections do not delay shutdown.
It has a five-minute deadline.
If automatic browser opening fails, open the URL printed to standard error.
The CLI verifies the signed ID token.
It confirms team access with Claimy before it saves a login.

Only the refresh credential and its server and identity binding are stored in the OS credential store.
The supported stores are macOS Keychain, Linux Secret Service, and Windows Credential Manager.
The regular `claimy/config.json` file under the OS user configuration directory stores only the default server URL.
Each authenticated command refreshes the ID token.
Rotating refresh credentials are saved under a per-server lock.
Logout removes the local credential and matching default URL.
It does not revoke the provider session.
Secure-storage failures stop login or refresh.
There is no plaintext fallback.

Browser login is disabled in the distribution configuration.
To enable it:

1. Register an OIDC **public client**.
   It must support S256 PKCE and a loopback callback at `http://127.0.0.1:<dynamic-port>/callback`.
   It must issue refresh credentials and ID tokens on refresh.
2. Set the manual REST issuer and JWKS URL.
   Set `claimy.auth.rest.audience` to this client's ID.
3. Set `claimy.auth.cli.enabled: true`.
   Set `claimy.auth.cli.client_id` to the same ID.
4. Include `openid` and `email` in `claimy.auth.cli.scopes`.
   Add `offline_access` if the provider requires it.
   Set only supported, non-secret consent options in `authorization_params`, such as `prompt`.

Providers that require a client secret are not supported by this public-client flow.
The CLI obtains public settings from the Claimy URL.
Users do not need to enter the issuer or client ID.
Production server and issuer URLs must use HTTPS.
Existing GitLab and Chat authentication is unchanged.
The native browser, macOS Keychain, and refresh path were exercised with a local signed OIDC fixture.
A deployed provider registration remains an operator prerequisite.

## GitLab CI caller

The native CLI uses a GitLab job ID token directly; it does not need `claimy auth login`.
Use a job image with a shell and the Claimy CLI installed:

```yaml
claimy_check:
  variables:
    CLAIMY_URL: "https://claimy.example.com"
  id_tokens:
    CLAIMY_ID_TOKEN:
      aud: "https://claimy.example.com"
  script:
    - claimy query --group payments --environments sandbox
```

Configure `claimy.auth.gitlab.issuer` and `jwks_url` for the trusted GitLab instance.
Set `claimy.auth.gitlab.audience` to the exact `aud` value requested by the job.
Claimy verifies the signed token, its expiry, stable user/project/job IDs, and the user's email against `claimy.auth.team_domain`.
Use `id_tokens`, not `CI_JOB_TOKEN`.
The CLI sends the token from `CLAIMY_ID_TOKEN` as a bearer credential.
A global `--token-file` takes precedence over that environment variable.
CI does not use the browser-login configuration or the OS credential store.
GitHub Actions OIDC is not supported by the current GitLab verifier.

For deployment jobs, acquire with `claimy acquire --output id`, proceed only on exit code `0`, and retain the exact claim ID for release.
Use separate stable request IDs for acquisition and release.
Replay is scoped to the GitLab issuer, project, and job; a GitLab job retry has a new job identity.
The token cannot be refreshed by the CLI and must still be valid when release runs.
Claim expiry is the fallback for interrupted jobs or expired tokens.
Claims belong to the GitLab user's canonical email.
Jobs for the same user do not conflict with each other's claims; this is not a strict per-job mutex.

Use [scripts/ci-acquire.sh](scripts/ci-acquire.sh) and [scripts/ci-release.sh](scripts/ci-release.sh).
The shell helpers remain available as an alternative to the CLI.
[scripts/gitlab-claim-example.yml](scripts/gitlab-claim-example.yml) shows ID-token and completion-cleanup configuration.

Set `CLAIMY_URL`, `CLAIMY_ID_TOKEN`, `CLAIMY_GROUP`, `CLAIMY_REQUEST_ID`, and `CLAIMY_ENVIRONMENTS`.
Set optional `CLAIMY_APP` and `CLAIMY_EXPIRES_AT` only when needed.
The acquire request ID must remain stable when the same logical CI request is retried.
A deliberate later attempt needs a new ID.

The acquire script makes one request.
It returns these exit codes:

- `0`: acquired and active.
  Standard output contains the claim ID.
- `1`: busy or an inactive successful replay.
  Do not deploy.
- `2`: invalid input, authentication, transport, storage, or response-shape failure.
  Do not deploy.

Preserve the claim ID for best-effort `after_script` release.
Release failure must not replace the deployment result.
Expiry handles interrupted jobs.
Never enable shell tracing around bearer tokens.

## REST API

[api/openapi.yaml](api/openapi.yaml) is the generated public API specification.
Its source is `api/openapi.yaml.gotempl` and the YAML fragments under `api/`.
The generated models and HTTP transport are committed in `pkg/client/client.gen.go`.

API calls require a bearer token.
The public browser-login configuration endpoint is the exception.
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
| GET | `/v1/auth/config` | Read public browser-login metadata. Return 404 when login is disabled. |
| GET | `/v1/auth/me` | Confirm an authenticated manual REST identity. Reject CI identities. |

Omit `app` to select a whole group.
A supplied empty or null app is invalid.
Use one or both values from `sandbox` and `prod` in `environments`.
Custom REST times require RFC3339 with an offset.
Catalog lists accept `{filter:{canonicalName:"api"},page:{limit:20,offset:0}}`.
The default and maximum page size is 100.
Catalog reads never register or mutate resources.

Busy acquisition returns HTTP 200 with `acquired:false`.
Validation, authentication, forbidden membership, missing catalog items, old history, and revision or request mismatches return 400, 401, 403, 404, 410, and 409.
Storage or identity-service failures return 5xx.
They never return busy.
An acquisition replay returns its original claim and expiry with current `activeNow`.
A busy replay remains busy.
Use a new request ID for a deliberate new attempt.

The framework health endpoint is `GET /health`:

```sh
curl --fail http://localhost:8088/health
```

A healthy process returns HTTP 200 and `{}`.

`GET /ready` checks MySQL reachability with a two-second deadline.
It returns HTTP 200 when the database responds and HTTP 503 when it does not.
Use `/ready` for readiness and `/health` for liveness.

The binary includes Berlin timezone data.
It handles SIGINT and SIGTERM shutdown.

## API client

The public Go client is `github.com/beeemT/claimy/pkg/client`.
It returns typed responses and structured `*client.APIError` values.
Its default HTTP timeout is 30 seconds.
It does not retry mutations.

Set `requestID` to a caller-generated value.
Generate it once for the logical mutation.
Reuse it for retries.

```go
api, err := client.New(serverURL, client.WithBearerToken(idToken))
if err != nil {
    return err
}
result, err := api.Acquire(ctx, client.AcquireRequest{
    Group:        "payments",
    Environments: []client.Environment{client.Sandbox},
    RequestId:    requestID,
})
```

## Google Chat

Configure a Chat app to send authenticated slash-command `MESSAGE` events to `POST /v1/chat/events`.
Use the public endpoint URL as the token audience.
Configure the allowed space names.
Events must include a human user name and team email.

```text
/claim take <sandbox|prod|both> <group> [app <app>] [until <Berlin time>]
/claim free <sandbox|prod|both> <group> [app <app>] [at <time>]
/claim list [<sandbox|prod>] [at <time>]
/claim release <claim-id>
/claim expiry <claim-id> until <Berlin time>
```

`free` queries availability.
It does not release a claim.
Replies show `free` separately from `allowedForCaller`.
They mark future results as projections.
Ambiguous or nonexistent Berlin local times require an explicit offset.
The app rejects unstructured commands.
It never infers intent with NLP.
Duplicate message delivery returns the saved mutation result.
Changed intent on the same message returns a conflict.

## Development

Install [Mise](https://mise.jdx.dev/) and Docker.
Run these commands from the repository root:

```sh
mise trust
mise install
mise run setup
mise run check
```

The setup task downloads public Go modules and installs Prek Git hooks.
An ordinary clone and a Git worktree use the same setup.
The `worktree-setup` task also runs setup for worktree tools.
Integration tests start disposable MySQL 8.0.42 containers.
They apply the real Goose migrations.
A separate lifecycle test verifies migration reversal and restoration.
The tests never use a shared database.

## Database and configuration

Claimy loads `config.dist.yml` from its working directory.
It listens on port 8088.
The committed distribution and test files contain example identities and development-only values.
Replace them before deployment.
Do not commit local configuration or credentials.

Use a dedicated MySQL database with InnoDB and enforced `CHECK` constraints.
Claimy requires MySQL 8.0.16 or later; MariaDB is not supported.
Later MySQL releases are accepted and do not need to match the integration test's 8.0.42 patch.
The SQLC connection must use `loc: UTC` and `time_zone: "'+00:00'".
Runtime startup rejects MariaDB, MySQL versions older than 8.0.16, non-UTC sessions, or an incomplete InnoDB schema.

Apply migrations explicitly to the selected database:

```sh
# Supply a dedicated database DSN through your local secret mechanism.
export GOOSE_DBSTRING
mise run migrate
mise run run
```

The migration task requires `GOOSE_DBSTRING`.
It selects the MySQL driver.
Runtime migrations are disabled by default.
The service prunes retained data daily.
It closes its database after HTTP shutdown completes.

Use environment variables to override configuration, for example:

```sh
SQLC_DEFAULT_URI_HOST=127.0.0.1 HTTPSERVER_DEFAULT_PORT=9090 mise run run
```

Supply `CLAIMY_DATABASE_PASSWORD` through your secret mechanism for a literal database password.
It takes precedence over the ordinary SQLC password setting after configuration decoding.
The server and catalog routes share this initialized client and connection pool.
Do not also put the Secret value in `SQLC_DEFAULT_URI_PASSWORD` or deployment configuration.

Configure these identity settings before normal use:

- `claimy.auth.team_domain`: the allowed account email domain.
- `claimy.auth.rest`: the trusted Google-style issuer, exact audience, and HTTPS JWKS URL.
- `claimy.auth.gitlab`: the trusted GitLab issuer, exact audience, and HTTPS JWKS URL.
- `claimy.auth.chat`: the Google issuer, endpoint-URL audience, and HTTPS JWKS URL.
- `claimy.chat.app_identity`, `allowed_spaces`, and `deadline`: the Chat app, allowed test/team spaces, and bounded event deadline.

JWT verification uses RS256.
It uses configured key URLs and normal TLS validation.
It checks exact issuer and audience values and token times.
GitLab ownership uses `user_email` and stable user, job, and project IDs.
`job_project_id` takes precedence when present.
A malformed present value is rejected.
Chat verifies `chat@system.gserviceaccount.com`.
It derives ownership from the event's authenticated human user.
The service account is never the claim owner.

Expose the service through an HTTPS ingress.
Do not log bearer tokens.
Keep existing deployment approvals and database backups.

## Checks and container smoke

Mise pins Go, gofumpt, golangci-lint, Prek, Gitleaks, Mockery, gotempl, oapi-codegen, Goose, actionlint, Helm, kind, and kubectl.
Run `mise run generate` after generator input changes.
`generate-check` rejects tracked and untracked generated drift.
`version-check` compares Go, template and code generators, Mockery, and Goose module and tool versions.
CodeRabbit review settings and path-specific contract guidance are in [.coderabbit.yaml](.coderabbit.yaml).
Automatic incremental reviews apply to non-draft pull requests.
Review settings do not change merge requirements.

```sh
mise run check
mise exec -- prek run --all-files
docker build --tag claimy:ci .
CLAIMY_IMAGE=claimy:ci mise exec -- go test -count=1 -tags=integration,fixtures ./test/e2e -run '^TestImageRuntimeIntegration$'
CLAIMY_IMAGE=claimy:ci mise run helm-smoke
```

The image smoke starts the actual container against disposable MySQL.
It uses an HTTPS JWKS fixture with a test CA.
It exercises the public client and native and container CLI.
It checks signed REST and Chat requests, persisted ownership and history, health, storage-failure exit codes, and graceful shutdown.
Normal integration tests skip the image smoke without `CLAIMY_IMAGE` and skip the opt-in Helm smoke.
`helm-smoke` requires a local image and the pinned tools. Missing prerequisites fail the task.
It installs and upgrades the chart in a disposable kind cluster with a private kubeconfig.
It checks migration ordering, literal Secret passwords, signed GitLab CLI and catalog requests, persisted job identity, and readiness during a verified MySQL outage and recovery.
The runtime image is static and nonroot.
It contains the safe configuration, Goose v3.24.3, migrations, CA bundle, and embedded timezone data.
It contains no shell or build tools.

## Continuous integration and publishing

GitHub Actions runs repository checks, the database-backed image smoke, and the real kind install/upgrade smoke.
Public actions are pinned to commit hashes.
Repository permissions are read-only for ordinary checks.
Pull requests do not use Docker Hub credentials or publish images.
After checks pass, `main` and valid Git-tag pushes publish to `docker.io/beeemt/claimy`.
The full commit hash and `latest` identify one image digest.
Tag pushes also publish the exact Git tag.
OCI labels retain the commit revision and release version.
Published images support `linux/amd64` and `linux/arm64`.
Stable tag pushes also publish the chart to `oci://registry-1.docker.io/beeemt/claimy-chart` after the image and package jobs succeed.
Chart version `X.Y.Z` uses image tag `vX.Y.Z`.
See the [chart guide](build/helm/claimy/README.md) for required values and Secret setup.

Archive packaging is read-only on pull requests.
It builds all four native targets with `CGO_ENABLED=0`.
Pull requests use snapshot version `0.0.0`.
Stable `v<major>.<minor>.<patch>` tags run checks for the selected tag before packaging and publishing.
Archives use the deterministic name `claimy_<version-without-v>_<goos>_<goarch>.tar.gz`.
Native archives are accompanied by `checksums.txt`.
The artifact set also contains `claimy-chart-<version-without-v>.tgz`.
The builder excludes macOS resource-fork metadata from the release archives.
The publishing job is separate.
Tag releases run automatically after the checks and package builds pass.
It checks `HOMEBREW_TAP_TOKEN` before publication.
It publishes the GitHub release archives, Helm chart archive, and native-archive checksums.
It updates `Formula/claimy.rb` in `beeemT/homebrew-tap` from the actual archives and their observed SHA256 values.

An authorized `workflow_dispatch` from the default branch may retry an existing stable tag.
The privileged job does not execute repository code selected by a tag.
It uses `ASKPASS` for the tap update.
The token is not placed in command arguments or a remote URL.
Rerunning an unchanged tap formula is a no-op.

To build the archives locally, install Python 3 and the Mise tools.
This command generates the four native archives, `claimy-chart-0.0.0.tgz`, native-archive `checksums.txt`, and `claimy.rb` under `dist/`:

```sh
mise run package-local v0.0.0
```

Version `0.0.0` is a local snapshot, not a published release.

### Maintainer release prerequisites

Complete all of these steps before expecting a public Homebrew install to work:

1. Merge the release changes into the default branch.
2. Configure the GitHub Actions secret `HOMEBREW_TAP_TOKEN`.
   Give it write access to `beeemT/homebrew-tap`.
   Do not use Docker Hub credentials for this token.
3. Create and push a new stable tag in the form `v<major>.<minor>.<patch>`.
   Do not reuse a tag that predates the CLI.
4. After the checks and package builds pass, the workflow publishes the release and updates the tap automatically.

No release environment or manual approval is required.
The release job must keep the tap token out of command arguments, remote URLs, logs, and generated files.
The workflow does not deploy the Claimy service.

Configure `DOCKERHUB_TOKEN` with Docker Hub **Read & Write** permissions.
Do not grant Delete access.
The username uses `DOCKERHUB_USERNAME` from an Actions secret or variable.
The default is `beeemt`.
Never put registry or tap credentials in source, build arguments, image layers, command arguments, remote URLs, or pull-request fixtures.

## License

[MIT](LICENSE).
