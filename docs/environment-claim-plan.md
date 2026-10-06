# Environment claims plan

## Scope and product decisions

Claimy provides one claim service for manual team use, GitLab CI jobs, and Google Chat. A claim reserves an app or its whole app group in `sandbox`, `prod`, or both environments. It uses database operation time as its start and becomes visible only after acquisition commits. It is never a future reservation. A group claim also covers apps registered in that group later.

The claim owner is the authenticated person's account email, not a request-body value.
For a GitLab job, use `user_email` from the validated ID token and retain stable identity claims.
For Chat, use the human user identity in the authenticated Chat event, not the Chat service account.
Compare the canonical account email snapshots from both authenticated providers.

Normalize surrounding whitespace and case consistently.
Do not treat plus-addresses, aliases, or different accounts as equal.
Reject requests without registration or a claim if authentication or team membership fails.

A claim may have a custom absolute expiry or a default expiry at 12:00 on the next calendar day in `Europe/Berlin`.
Manual and CI claims use the same default.
All claims expire. There is no unbounded claim.
A custom REST expiry must use RFC3339 with an explicit offset.

Chat accepts an exact Berlin local time.
Reject nonexistent or ambiguous local times unless the user supplies an explicit offset.
Expiry must be strictly later than database UTC time at acquisition or edit.

Same-owner overlap is allowed.
Each claim has its own ID, expiry, history, and release.
An overlapping acquisition does not extend or replace an existing claim.
An active claim from another owner conflicts if it overlaps.

Report availability (`free`) separately from caller permission (`allowedForCaller`).
An overlapping same-owner claim makes the target taken, but permits that owner's CI job.
Do not add a same-owner CI execution mutex.

Any authenticated team member may release or change any claim's expiry, whether its source is manual or CI.
No administrator role or special claim-management permission is required.
This service supports team coordination, not strict compliance enforcement.
Record the actor.
Claim scope and owner never change.
An expired claim cannot be revived by an expiry edit.

A new acquisition is required.
No web UI, deploy serialization/fencing, reminders, notifications, Redis, or generic retry framework is in scope.
A CI claim is an advisory lease.
Expiry or a team member's release does not fence a deployment process that is already running.
Existing production deployment protections remain necessary.

Keep query history for 90 days. A query may ask for the current state, a historical instant inside retention, or a future projection of claims accepted now. A future projection is not a reservation or guarantee. A request older than retention is unavailable, never evidence that a target is free. Unknown-resource queries do not register anything.

## Repository evidence

The existing `LICENSE` is MIT and remains unchanged.
The repository implements the claim domain, MySQL persistence, authenticated REST and CI adapters, and Google Chat commands.
`README.md` contains the current setup and operation commands.

The current module pins Go 1.27.0, Gosoline 0.65.3, and `httpserver` 0.6.4.
The implementation uses the public SQLH 0.8.0, SQLR 0.9.1, and SQLC 0.4.0 baseline.

| ID | Pattern | Public source | Symbol |
|---|---|---|---|
| E1 | Typed CRUD handler construction | [sqlh v0.8.0 `handlers_crud.go`](https://github.com/gosoline-project/sqlh/blob/v0.8.0/handlers_crud.go#L247-L303) | `sqlh.NewCrudHandler` |
| E2 | Typed CRUD definition | [sqlh v0.8.0 `handlers_crud.go`](https://github.com/gosoline-project/sqlh/blob/v0.8.0/handlers_crud.go#L100-L220) | `sqlh.CrudDefinition` |
| E3 | Transaction-aware CRUD operation extension point | [sqlh v0.8.0 `operation.go`](https://github.com/gosoline-project/sqlh/blob/v0.8.0/operation.go#L9-L18) | `sqlh.CrudOperation` |
| E4 | Transaction-scoped row-lock query clause | [sqlr v0.9.1 `qb_select.go`](https://github.com/gosoline-project/sqlr/blob/v0.9.1/qb_select.go#L127-L143) | `sqlr.QueryBuilderSelect.ForUpdate` |
| E5 | Explicit route registration | [httpserver v0.6.4 `router.go`](https://github.com/gosoline-project/httpserver/blob/v0.6.4/router.go#L14-L132) | `httpserver.RouterFactory`; `httpserver.Router.HandleWith`, `httpserver.Router.Handle`, and HTTP-method helpers |
| E6 | SQL client provider | [sqlc v0.4.0 `client.go`](https://github.com/gosoline-project/sqlc/blob/v0.4.0/client.go#L39-L80) | `sqlc.ProvideClient` |
| E7 | Client transaction callback | [sqlc v0.4.0 `client.go`](https://github.com/gosoline-project/sqlc/blob/v0.4.0/client.go#L176-L207) | `sqlc.Client.WithTx` |
| E8 | Transaction-aware repository constructor | [sqlr v0.9.1 `repository_tx.go`](https://github.com/gosoline-project/sqlr/blob/v0.9.1/repository_tx.go#L11-L67) | `sqlr.NewRepositoryTxWithSettings` |
| E9 | Transaction runner | [sqlh v0.8.0 `transaction.go`](https://github.com/gosoline-project/sqlh/blob/v0.8.0/transaction.go#L26-L139) | `sqlh.TxRunner.Run` |
| E10 | Server error handling and mapping options | [httpserver v0.6.4 `server_options.go`](https://github.com/gosoline-project/httpserver/blob/v0.6.4/server_options.go#L32-L58) | `httpserver.WithErrorMapper`, `httpserver.WithErrorHandler` |

These release-pinned sources establish library capabilities only.
Claimy owns its domain rules, group serialization, expiry and overlap decisions, HTTP error taxonomy, and whole-transaction retry policy.
In particular, SQLC `Client.WithTx` does not retry whole transactions, and SQLR `ForUpdate` does not by itself establish Claimy's group-wide serialization invariant.
These references do not establish implementation of Claimy's domain rules.

## Repository setup and tooling

The repository has public setup, generated API models and mocks, Goose migrations, isolated MySQL tests, container packaging, and GitHub Actions.
An ordinary clone and a worktree use the same setup without private module replacements or worktree-specific dependencies.
See `README.md` for current commands.

Go is pinned identically in `go.mod` and Mise (`1.27.0`).
The active Mise tools are:

```toml
[tools]
go = "1.27.0"
gofumpt = "0.7.0"
golangci-lint = "2.13.1"
prek = "0.4.11"
gitleaks = "8.30.1"
mockery = "3.7.0"
oapi-codegen = "2.4.1"
"go:github.com/pressly/goose/v3/cmd/goose" = "3.24.3"
```

Use stock golangci-lint v2 with `linters.default: none`, `gofumpt` as the formatter, tests enabled, build tags `integration,fixtures`, readonly module downloads, concurrency 4, and a five-minute timeout.
Enable these built-in linters explicitly: `dogsled`, `dupword`, `errcheck`, `gocognit`, `goconst`, `gocritic`, `godox`, `govet`, `ineffassign`, `lll`, `misspell`, `nestif`, `nlreturn`, `nolintlint`, `revive`, `staticcheck`, `unused`, `usetesting`, and `whitespace`.
Exclude `dogsled`, `goconst`, and `lll` from test files, and exclude Revive's flag-parameter rule from test code. Keep other exceptions narrow and explicit.
Do not require an unpublished or custom linter bundle.
Generator tools use real interfaces and API contracts.
Mockery v3.7.0 generates testify mocks in per-package `mocks/` directories.
oapi-codegen v2.4.1 generates typed models from static OpenAPI source.
`go generate ./...` is authoritative. Matching Go tool directives and generated-drift checks are implemented.

Goose v3.24.3 validates timestamped `Up`/`Down` SQL migrations under `build/migrations/claimy/`.
Disposable MySQL tests apply and reverse them.
No default task may apply or roll back migrations on a shared database.
Integration tests use Docker, the public disposable MySQL harness, deterministic fixtures, and a fixed test clock.
Do not add a second MySQL CI service unless the harness requires it.

| File or path | Current status |
|---|---|
| `cmd/claimy/main.go`, `config.dist.yml` | Authenticated claim, catalog, Chat, and health routes with shared database lifecycle |
| `mise.toml`, `scripts/` | Pinned tools, generators, migration/check tasks, and real CI caller scripts |
| `go.mod`, `go.sum` | Public dependencies and matching generator tool directives |
| `.golangci.yml` | Stock v2 config and explicit built-in linters |
| `prek.toml`, `.gitleaks.toml` | Formatting, lint, file checks, and secret scanning |
| `.gitignore`, `.dockerignore` | Exclude local credentials, configuration, build output, and index artifacts |
| `.github/workflows/ci.yml` | Public checks, real-image/MySQL smoke, and gated Docker Hub publishing |
| `Dockerfile` | Public multi-stage build and nonroot static runtime |
| `README.md` | Current contributor setup, runtime, and publishing instructions |
| Existing `LICENSE` | Unchanged MIT license |
| Generator inputs, `config.test.yml`, migrations, integration tests | Implemented with real API/interfaces, safe examples, and disposable signed/MySQL fixtures |

The following Mise tasks are implemented and shared by local development and CI:

| Task | Current command or behavior |
|---|---|
| `setup` | `go mod download && prek install` |
| `worktree-setup` | Depend on `setup` |
| `version-check` | Compare Go, Mockery, oapi-codegen, and Goose module/tool versions |
| `fmt` | `gofumpt -w .` |
| `fmt-check` | Reject files reported by `gofumpt -l .` |
| `vet` | `go vet ./...` |
| `lint` | `golangci-lint run` |
| `test` | `go test ./...` |
| `test-integration` | `go test -tags=integration,fixtures ./...` against disposable MySQL |
| `generate` | `go generate ./...` |
| `generate-check` | Regenerate and reject tracked or untracked generated drift |
| `migrate-validate` | Validate real timestamped Goose migrations without a database |
| `migrate` | Apply migrations only with an explicit dedicated `GOOSE_DBSTRING` |
| `secret-scan` | Redacted Gitleaks scans of all Git refs and working files |
| `build` | Build `build/bin/claimy` |
| `run` | `go run ./cmd/claimy` |
| `check` | Version, format, vet, lint, unit/integration, generation, migration, secret, and build checks |

The image-specific test requires `CLAIMY_IMAGE` and executes the actual binary against isolated MySQL and HTTPS identity fixtures.

Contributor onboarding is `mise trust`, `mise install`, `mise run setup`, then `mise run check`.
Commit only safe example configuration and use Gitleaks defaults without copied broad allowlists.
The workflow uses public actions pinned to commit hashes on Docker-capable Linux runners.
Pull requests, `main` pushes, and Git-tag pushes run the same checks and database-backed image smoke.
Repository permissions are `contents: read`; Docker Hub authentication uses its own token.
There are no private pipeline includes, private base images, or automatic deployments.
Repository publishing CI is separate from GitLab job-identity support in the claim API.
The image includes the binary, safe configuration, migrations, CA certificates, and embedded Berlin timezone data.
Helm, protobuf, and GitOps tools remain conditional on real deployment or generation requirements.
Codegraph and Git-work are optional local tools, not CI prerequisites.

### Docker Hub publishing

Publish to `docker.io/beeemt/claimy` in the personal [Docker Hub repository](https://hub.docker.com/repository/docker/beeemt/claimy/general).
The login uses `DOCKERHUB_USERNAME` from an Actions secret or repository variable, with `beeemt` as its default.
Store the Docker Hub token in the Actions secret `DOCKERHUB_TOKEN`.
Use a Docker Hub personal access token with **Read & Write** permissions.
Do not grant Delete or administrative access.
The account must own the image repository or have write access to it.
Follow the [Docker personal access token instructions](https://docs.docker.com/security/access-tokens/personal-access-tokens/) to create the token.
Keep credentials out of the source, build arguments, image, and logs.
Keep Docker Hub repository visibility as configured.

Run the publishing job only after all checks pass.
Allow publishing for `push` events on `main` and Git tags (`tags: ['**']`).
Pull requests run checks without registry login, registry secrets, or publishing.
Do not use `pull_request_target` to publish pull-request code.
Do not add automatic deployment jobs.

Use the full commit hash from `git rev-parse HEAD` after checkout.
For an annotated Git tag, use its checked-out commit, not the tag object's hash.
Build once per publishing job and apply all requested image tags to that build.

| Trigger | Published image tags | OCI image labels |
|---|---|---|
| Push to `main` | `docker.io/beeemt/claimy:<full-commit-sha>` and `docker.io/beeemt/claimy:latest` on the same image | `org.opencontainers.image.revision=<full-commit-sha>` and `org.opencontainers.image.version=<full-commit-sha>` |
| Git-tag push, for example `v1.2.3` | `docker.io/beeemt/claimy:<full-commit-sha>`, `docker.io/beeemt/claimy:v1.2.3`, and `docker.io/beeemt/claimy:latest` on the same image | Revision is the full commit hash. Version is the Git tag, including its leading `v`. |
| Pull request or failed checks | None | No image publication |

The registry references above are Docker image **tags**.
Also set the OCI **labels** so the image records its source commit and release version.
Update `latest` on every successful `main` or Git-tag publication.
It is a moving alias for the most recently published image, not a fixed source version.
Do not add branch names, shortened hashes, or extra version aliases.
Release Git tags must be valid Docker image tag names so the image tag can retain the exact Git tag.

Use the public Docker login, Buildx, metadata, and build/push actions.
The [Docker publishing example](https://docs.docker.com/build/ci/github-actions/push-multi-registries/) documents Docker Hub credentials and the build/push action.
The [metadata action](https://github.com/docker/metadata-action#customizing) documents image tags, OCI labels, and the automatic `latest` default.
Set `latest=true` so every publishing run includes that alias.
Pass the resolved full commit hash as a raw tag instead of the default shortened, `sha-`-prefixed tag.
The publishing job uses these metadata inputs:

```yaml
images: docker.io/beeemt/claimy
flavor: latest=true
tags: |
  type=raw,value=${{ steps.commit.outputs.sha }}
  type=ref,event=tag
labels: |
  org.opencontainers.image.revision=${{ steps.commit.outputs.sha }}
```

The `commit` step exports `git rev-parse HEAD` as its `sha` output.
The metadata action uses the Git tag for its version label when present, otherwise the raw commit hash.
Pass both metadata outputs, `tags` and `labels`, to the build/push action.
Verify all published aliases resolve to the same pushed digest, including `latest`.
The workflow is implemented. Registry publication must be verified from a successful publishing run; local checks alone are not proof of publication.

## Architecture

Propose a small Go/Gosoline HTTP service using the selected public dependency baseline [E1-E10].
Use MySQL/InnoDB, SQLC transactions, SQLR repositories/query builders where practical, and SQLH for read-only catalog queries.
These public library APIs are available building blocks, not requirements imposed by existing Claimy files.

Claimy will share one domain service across REST handlers and the Google Chat event handler.
Chat calls the same domain methods.
It does not call Claimy's public HTTP endpoint from inside a transaction.

Use four boundaries:

1. **Identity adapters** validate GitLab ID tokens, authenticated REST team-member identity, and Google Chat request identity.
They produce one internal actor with canonical account email, issuer, stable subject, source, and team membership.
No endpoint accepts an owner email or owner ID from a request body.
2. **Domain service** validates scope and expiry, computes conflicts, and exposes `Acquire`, `Query`, `Release`, and `ChangeExpiry`.
It owns idempotency semantics and calls the persistence layer.
Manual REST, CI REST, and Chat use the same rules.
3. **MySQL repository** owns schema mapping and transactional acquisition, change, and release operations.
Use an SQLC transaction with SQLR/raw SQL where needed.
Use it for the parent lock and current-read conflict query.
Claimy-owned composition can obtain the SQL client through `sqlc.ProvideClient` and construct transaction-aware SQLR repositories with `sqlr.NewRepositoryTxWithSettings` where appropriate [E6,E8].
SQLC `Client.WithTx` and SQLH `TxRunner.Run` provide transaction callback mechanisms [E7,E9]; they do not implement Claimy's group protocol or domain transitions.
Claimy owns whole-transaction retry behavior; `Client.WithTx` does not retry whole transactions.
For HTTP, use `RouterFactory` and explicit `Router.HandleWith`/HTTP-method registration [E5].
Configure HTTP error mapping and handling with `WithErrorMapper`/`WithErrorHandler` [E10], while Claimy owns authentication, route policy, and its domain error taxonomy.

Serialize all writes for a group on its permanent `app_groups` row.
Each writer upserts the group by its canonical unique name.
It then locks that row with `SELECT ... FOR UPDATE`.
Next, it registers an app if needed and evaluates and writes the claim in the same transaction.
App registration must use the same group lock.
The parent lock exists even when the group has no app or claim rows.

A child-claim lock cannot reliably serialize a target that does not exist yet.
It also cannot prevent a competing app registration or claim phantom.
The parent lock serializes both environments together, so `sandbox` plus `prod` cannot partially succeed.
Unrelated groups remain independent.

After acquiring the group lock, read `UTC_TIMESTAMP(6)` once.
Use that operation time for expiry validation, claim timestamps, and history.
Use a locking/current read for active claims and their environments in the same transaction.
InnoDB `SELECT ... FOR UPDATE` reads the current committed rows after waiting, not a stale repeatable-read snapshot.
Do not depend on child-row locks alone.

On a retryable DB failure, retry the whole transaction.
Do not retry statements within a partially run transaction.
Claimy-owned retry starts a new transaction, reacquires the group lock, and recomputes operation time; `Client.WithTx` does not retry the whole transaction [E7].

`Acquire` commits catalog registration even on a business conflict, but inserts no claim when busy.
A successful acquisition commits the claim, its environment rows, initial history version, and idempotency result together.
For invalid requests, failed authentication, and storage failures, do not commit catalog writes.
The permanent group-row lock is released at transaction end.
The persisted claim, not a held DB lock, is the lease.

## Claim/conflict rules

A claim has one `group_id` and a nullable `app_id`.
`NULL` means the whole group.
It has one or two environment rows, an immutable owner email and source, and an opaque claim ID.
It also has creation and expiry timestamps, an optional release timestamp, and a revision.
`manual` and `ci` are the only claim sources.

An app belongs to exactly one group.
Use canonical lower-case technical slugs for group and app names.
Normalize names consistently and reject malformed names.
Environment names are exactly `sandbox` and `prod`.
Reject other values instead of registering them.

Two claims overlap only if they refer to the same group and share at least one environment.
At least one claim must cover the group, or both claims must refer to the same app.
An overlapping claim conflicts only when its canonical owner email differs from the caller's account email.
A claim is active at time `t` iff it is not released and `t < expires_at`.
At the exact expiry instant, it is inactive.

| Requested target | Active claims that make it taken | Other-owner claims that block acquisition |
|---|---|---|
| Whole group | Any active group claim or app claim in the selected environment set | Any active group claim or app claim in that set owned by another email |
| One app | Any active group claim or claim for that same app in the selected environment set | Any active group claim or same-app claim in that set owned by another email |

“Taken” and “blocks this caller” are deliberately different.
A same-owner claim makes `free=false` and sets `allowedForCaller=true`.
It does not block a new independent claim from that owner.
An overlapping different-owner claim makes `free=false` and `allowedForCaller=false`.
It makes acquisition return busy.

Conflicts include the existing claim ID, owner, app/group scope, environment intersection, source, and expiry so CI and Chat can report the reason.
A group claim covers an app registered in the future because conflict and query rules match by `group_id`, not by a registration-time membership snapshot.

The acquisition request contains one environment or both normalized environments.
The API accepts `environments:["sandbox","prod"]`.
Chat uses `both`.
Duplicate, empty, or unsupported selections are invalid.
A busy both-environment request returns the blocking claims.
It commits no claim or partial environment rows.
It never acquires only one environment.

Release addresses one claim ID and never releases other overlapping claims.
An expiry update preserves the claim ID, scope, owner, source, and environments.
It increments only the revision.

Any authenticated team member can release or change any claim's expiry, including another member's manual or CI claim.
The same permission applies through REST and Google Chat.
Every action records the actor and channel.
An expiry edit requires the expected revision and an active claim.

A stale revision or inactive/expired claim returns a conflict.
It cannot resurrect the claim.
A same-value expiry edit is a no-op and adds no history revision.
Releasing an already inactive claim returns an inactive result without changing its history.
Neither operation accepts a replacement scope or owner.

## Expiry

If `expiresAt` is omitted, compute 12:00 on the next calendar day in the IANA zone `Europe/Berlin`.
Convert the locked transaction's DB operation time to that zone.
Add one local calendar day and construct local noon.
Do not add 24 hours.
Manual and CI requests use the same function.

Include suitable Go timezone database data for `Europe/Berlin` in the built application.
Do not silently fall back to a fixed UTC offset.

REST custom expiry must use RFC3339 with an explicit offset and is normalized to UTC.
The Chat parser accepts an exact Berlin local date and time.
Reject nonexistent or ambiguous local times without an explicit offset.
An expiry at or before DB operation time is invalid.
Roll back any provisional catalog registration on this failure.

There is no maximum lifetime cap.
Every claim has a finite expiry.

Check expiry on reads and writes.
Do not rely on a timer that must run on time.
The active predicate is `released_at IS NULL AND expires_at > operation_time`.
`S10` and `S11` cover the equality edge and both Berlin DST transitions.
They also prove that CI and manual acquisition share the default calculation.

For a current query, read current claim state in one coherent database snapshot.
For a past query, select the claim version whose validity interval contains the requested instant.
Also require the requested instant to be earlier than that version's `expires_at`.
Require that the version is not released.
Use half-open intervals: `valid_from <= at AND (valid_to IS NULL OR at < valid_to)`.

At an exact expiry or release time, the claim is inactive.
If multiple revisions share one microsecond, order by revision and return the last state.
Never treat zero-length versions as valid at that instant.

A future query evaluates the current accepted version of each claim at the requested future instant.
It excludes acquisitions that have not happened and cannot prevent them.
Return `projected:true` and the requested `at` so clients do not present the result as a reservation.
A past query earlier than DB time minus 90 days returns an explicit unavailable result (HTTP 410), never an empty/free result.

Retain a version that spans the cutoff and all current state needed for a long-running claim.
For retained claims, prune only closed versions ending before the cutoff. Remove older terminal claims under the retention rules below.
It must not cascade-delete an active claim or a version needed for an in-retention query.

## MySQL schema

Use InnoDB, UTC `DATETIME(6)` values, foreign keys with `ON DELETE RESTRICT`, and explicit Goose SQL migrations.
Require MySQL 8.0.16 or later to enforce `CHECK` constraints.
Configure and verify the production server version.
Use the same version for isolated integration tests.
The app owns canonical slug and email normalization.

The schema below is a proposed contract, not an existing implementation.
Use opaque UUID strings for public claim IDs and internal unsigned numeric IDs for catalog rows.

| Table | Columns and constraints | Indexes and purpose |
|---|---|---|
| `app_groups` | `id BIGINT UNSIGNED PK AUTO_INCREMENT`; `canonical_name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE`; `created_at DATETIME(6) NOT NULL` | Unique canonical name is the group registration/upsert key and permanent serialization row. No rename/delete API. |
| `apps` | `id BIGINT UNSIGNED PK AUTO_INCREMENT`; `group_id BIGINT UNSIGNED NOT NULL FK app_groups(id)`; `canonical_name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL`; `created_at DATETIME(6) NOT NULL`; `UNIQUE(group_id,canonical_name)` and `UNIQUE(group_id,id)` | Group-scoped app uniqueness and composite target FK. No reparent/delete API. |
| `claims` | `id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PK`; `group_id BIGINT UNSIGNED NOT NULL FK app_groups(id)`; nullable `app_id BIGINT UNSIGNED`; `owner_email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL`; `source ENUM('manual','ci') NOT NULL`; nullable `gitlab_issuer VARCHAR(255)`, `gitlab_project_id VARCHAR(128)`, `gitlab_job_id VARCHAR(128)`, `gitlab_user_id VARCHAR(128)`; `created_at`, `expires_at` `DATETIME(6) NOT NULL`; nullable `released_at DATETIME(6)`; `revision INT UNSIGNED NOT NULL`; composite FK `(group_id,app_id)` to `apps(group_id,id)`; checks `expires_at > created_at`, `released_at IS NULL OR released_at >= created_at`, and CI identity fields are all present iff `source='ci'` | `(group_id,app_id,released_at,expires_at)` and `(group_id,released_at,expires_at)` for group/app active-claim lookups. No unique active-target key: intentional same-owner overlap is allowed. |
| `claim_environments` | `claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL FK claims(id)`; `environment ENUM('sandbox','prod') NOT NULL`; `PRIMARY KEY(claim_id,environment)` | `(environment,claim_id)` supports environment-first conflict/query lookups. One row per environment; insert both rows atomically. |
| `claim_versions` | `claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL FK claims(id)`; `revision INT UNSIGNED NOT NULL`; `valid_from DATETIME(6) NOT NULL`; nullable `valid_to DATETIME(6)`; `expires_at DATETIME(6) NOT NULL`; `released BOOLEAN NOT NULL`; `actor_email VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL`; `actor_issuer VARCHAR(255) NOT NULL`; `actor_subject VARCHAR(128) NOT NULL`; `channel ENUM('rest','gitlab_ci','google_chat') NOT NULL`; `action ENUM('acquired','expiry_changed','released') NOT NULL`; `PRIMARY KEY(claim_id,revision)`; check `expires_at > valid_from`, `valid_to IS NULL OR valid_to >= valid_from`; generated `open_marker TINYINT GENERATED ALWAYS AS (IF(valid_to IS NULL,1,NULL)) STORED` with `UNIQUE(claim_id,open_marker)` | `(claim_id,valid_from,revision)` and `(valid_from,valid_to,claim_id,revision)` support per-claim and as-of queries. The generated unique marker permits many closed versions but only one open version. A closed row is immutable except for setting `valid_to` once. |
| `request_results` | `principal_kind ENUM('gitlab_job','rest_user','google_chat_user') NOT NULL`; ASCII `principal_issuer VARCHAR(255) NOT NULL`, `principal_id VARCHAR(128) NOT NULL`, `request_id VARCHAR(128) NOT NULL`; `payload_sha256 BINARY(32) NOT NULL`; `outcome ENUM('acquired','busy','released','expiry_changed') NOT NULL`; nullable `claim_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin FK claims(id)`; `response_json JSON NOT NULL`; `created_at`, `retain_until DATETIME(6) NOT NULL`; primary key `(principal_kind,principal_issuer,principal_id,request_id)` | `retain_until` index for bounded cleanup. Stores the original outcome/response and makes the idempotency key unique across groups. |

Use a consistent app-level canonical form for `owner_email`.
Compare only the authenticated account email snapshot and do not infer aliases.
CI identity columns are null for manual claims.
Insert a `claim_versions` row as part of each claim mutation.
On change or release, close the open version at the DB operation timestamp.

Then append revision `n+1` and update the current `claims` projection in the same transaction.
For a same-microsecond transition, the closed version has `valid_to=valid_from`.
The half-open query does not select that version.
Revision order identifies the final state.

A `request_results` row stores the canonical payload hash and original result JSON.
Use an authenticated, stable actor key.
For GitLab, include issuer, project, and job.
For REST, include issuer and subject.
For Chat, include issuer, user, and the message-derived request key defined below.

Identical replays return the original outcome.
Acquisition replays also return current `activeNow`.
A different payload with the same key returns 409.
If a cross-group request loses the unique-key race, roll back provisional registration before reading the winning result.

Keep busy results for 90 days.
Keep successful results while the claim remains active and for 90 days after its terminal time.
Recompute their retention boundary after expiry changes and release.
Terminal time is the earlier of expiry and release.
After result retention, the original request key is no longer replay-safe.
Clients must not intentionally reuse an old key.

Expiry does not depend on cleanup.
A daily retention task uses the same group-lock protocol as claim mutations.
It first deletes expired replay results.
It may then delete a terminal claim only when its terminal time is older than the 90-day cutoff.
Also require that no replay result references the claim.

Delete its environment and version rows before the claim to respect foreign keys.
This includes the final open version of an expired or released claim.

For active or recently terminal claims, remove only closed versions ending before the cutoff.
Preserve versions spanning the cutoff and the state needed for in-retention queries.
Preserve catalog rows.
The query path enforces the cutoff even if a cleanup run is delayed.
Terminal history does not remain indefinitely, while long-running claims remain queryable.

## sqlh API

Use SQLH `NewCrudHandler` with `CrudDefinition` for read-only catalog list/read paths and mapping [E1,E2].
Register only safe read/list routes for groups, apps, and catalog projections.
Do not mount generic SQLH create, update, patch, or delete routes for claims or catalog resources.
A generic create must not bypass the group lock, conflict check, environment atomicity, history insert, or idempotency record.

Implement acquisition, release, expiry-change, and claim-query endpoints as explicit typed `httpserver` handlers.
Have them call the shared domain service.
Run the group-level changes in a SQLC transaction and use SQLR/raw SQL for the parent lock and current-read query.
SQLH `CrudOperation` is the public transaction-aware extension signature for replacing an operation [E3]; any claim-specific checks and mutations inside that operation remain Claimy-owned.
Use SQLR `QueryBuilderSelect.ForUpdate` only as a query clause inside the transaction [E4]; it does not establish Claimy's group-level serialization by itself.

Register routes through `RouterFactory` and explicit `Router.HandleWith`/HTTP-method helpers [E5].
Use `WithErrorMapper` and `WithErrorHandler` for HTTP error mechanics, while Claimy defines its status taxonomy [E10].

| Method and path | Request | Result |
|---|---|---|
| `POST /v1/claims/acquire` | `{group, app?, environments:["sandbox","prod"], expiresAt?, requestId}`; select one or both environments; omitted `app` means whole group | HTTP 200 with `{acquired:true,claim:{id,scope,environments,ownerEmail,expiresAt,revision,activeNow:true}}`, or HTTP 200 `{acquired:false,conflicts:[...]}` for a busy result only. |
| `POST /v1/claims/query` | Read filters for group/app/environment and optional RFC3339 `at` | Current, retained historical, or current-state future projection; include `known`, `at`, `projected`, `free`, `allowedForCaller`, and matching/inherited claim details. Never registers a resource. |
| `POST /v1/claims/{id}/release` | Mutation request ID | Any authenticated team member may release exactly that manual or CI claim. Append actor history, or report it already inactive. No owner-only or administrator permission check. |
| `PATCH /v1/claims/{id}` | `{expiresAt,expectedRevision,requestId}` | Change only expiry, validate active state and revision, and append history. |
| `POST /v1/catalog/groups/query`, `GET /v1/catalog/groups/{group}`, `POST /v1/catalog/groups/{group}/apps/query` | Read-only pagination/filter or one catalog identity | SQLH list/read view. No endpoint registers or edits catalog data. |

Require request IDs for every mutating API action.
The acquire JSON field is `requestId`.
Release and expiry requests use the same field.
Sort environments before hashing.
Normalize slugs and convert explicit offset expiry to UTC before hashing.

Preserve the difference between omitted/default expiry and explicit expiry.
Compute default expiry only on the first execution.
Identical retries return the stored first result, including its claim ID and expiry.
They do not compute a new default expiry.

A busy replay stays busy even if the resource later becomes free.
Use a new request ID to make a new acquisition attempt.

Reject a body-supplied owner field instead of trusting or silently using it.
Return 400 for validation errors, including malformed times, unsupported environments, and invalid scopes.
Return 401 for missing or invalid authentication.
Return 403 for an authenticated caller outside the configured team.

Return 404 when a non-mutating catalog read lacks an item.
Return 410 for old history.
Return 409 for stale revisions or request IDs reused with different payloads.
Busy acquisition is not an HTTP error.

Return 5xx for database and service failures.
Never map those failures to `acquired:false`.
Central error mapping must preserve this distinction.

## CI locking

Validate GitLab ID tokens with a fixed issuer, exact audience, signature, expiry, and not-before checks.
Check the configured team email domain before registration. Do not add claim-owner, administrator, project-specific, or production-compliance roles.
Derive ownership from `user_email`, not a CI variable or JSON owner field.
Record stable user, project, and job IDs.

[GitLab's ID-token reference](https://docs.gitlab.com/ci/secrets/id_token_authentication/) documents `user_email`, `user_id`, and `job_id`.
Email is mutable, so it cannot be the only trust condition.
Verify the deployed GitLab version's claims before enabling CI.
Use stable project and job identity claims supported by the deployed GitLab version.
Use the existing GitLab and Google account identities. Do not add a separate confirmed-email compliance gate.
Token validation and team membership remain required. Ownership equality compares account emails from the authenticated providers.

The request contains the app-group slug, optional app slug, selected environments, and a unique request ID generated by the job.
If expiry is absent, use the same next-day Berlin noon calculation as manual claims.
Same-email CI jobs may each acquire distinct claims and proceed.
They do not serialize one another.
Only a successful acquisition permits deployment.
A successful replay permits deployment only when `activeNow` is true.
Busy returns HTTP 200 with `acquired:false`.
Auth, validation, revision, network, and storage errors are distinct failures that block deployment.
Never translate a transport, auth, or storage error to busy or false.

The caller must make one explicit acquisition request and branch on HTTP status and JSON.
It must not build a retry loop.
Use a unique request ID for a deliberate new attempt.
A same-ID retry after a lost response returns the original result.
Release the returned claim ID after deployment in a best-effort `after_script`.
Expiry is the fallback if the runner stops.
Do not let release failure erase the deployment result.
This lease cannot guarantee fencing after expiry or a team member's early release.
Claimy does not replace or add production deployment approval rules.

The implemented caller is `scripts/ci-acquire.sh`.
`scripts/ci-release.sh` handles completion cleanup.
`scripts/gitlab-claim-example.yml` configures the ID token and preserves the claim ID for `after_script`.
Use Bash, `curl`, and `jq`.
Set `CLAIMY_URL`, `CLAIMY_ID_TOKEN`, `CLAIMY_GROUP`, `CLAIMY_REQUEST_ID`, and `CLAIMY_ENVIRONMENTS`.
For example, use `CLAIMY_ENVIRONMENTS='["sandbox","prod"]'`.
Omit `CLAIMY_APP` for the whole group and `CLAIMY_EXPIRES_AT` for default expiry.
Generate and preserve one request ID before the request. Disable shell tracing.

Exit 0 returns the claim ID and permits deployment.
Exit 1 means busy or an inactive replay. Exit 2 means a request failure.
The script sends exactly one acquire request and never retries automatically.
Release the successful claim with a new mutation request ID.
An expired job token can make cleanup fail. Expiry remains the fallback.


## Google Chat commands

Expose one HTTPS Chat event endpoint. Follow [Google's request-verification contract](https://developers.google.com/workspace/chat/verify-requests-from-chat).
Use the configured endpoint-URL audience. Verify signature, audience, issuer, token times, verified email, and `chat@system.gserviceaccount.com` service identity.
This email identifies Chat, not the claim owner.

Read the human identity from the authenticated event. Require a usable user email, stable user ID, and configured Workspace team membership.
The [Chat User reference](https://developers.google.com/workspace/chat/api/reference/rest/v1/User) shows the available identity fields.
Reject mutations with missing required identity fields. Never use display names or typed owner text as a fallback.
Chat claims use the same account email normalization and team membership rules.
Any authenticated team member may release or change any manual or CI claim through Chat.
No special permission, owner match, or originating-job check is required for those operations.
Use strict structured slash commands.
Do not infer intent with NLP:

- `/claim take <sandbox|prod|both> <group> [app <app>] [until <Berlin time>]` acquires a manual claim. Omit `app` for the whole group. `both` is all-or-none.
- `/claim free <sandbox|prod|both> <group> [app <app>] [at <time>]` is a read-only availability query, not a release. Show `free` and `allowedForCaller` separately.
- `/claim list [<sandbox|prod>] [at <time>]` lists claims and supports retained history/current-state projection.
- `/claim release <claim-id>` releases exactly one claim.
- `/claim expiry <claim-id> until <Berlin time>` changes only expiry. Resolve the current revision under the group lock on the first execution.

Require explicit scope and environment when omission is ambiguous.
Busy replies show the blocking owner, scope, environment, claim ID, and expiry.
List replies show the owner, scope, start, expiry, source, and inherited group coverage.
Show `free` separately from same-owner permission.
Mark future results as projections.
Confirm mutations only after commit.

Use slash commands received as `MESSAGE` events.
The [Google Chat Event schema](https://developers.google.com/workspace/chat/api/reference/rest/v1/Event) has no generic event-ID field.
Derive `requestId` from a SHA-256 hash of the configured Chat app identity, `space.name`, and `message.name`.
Scope it to the authenticated `user.name`.
Require these fields for mutations.
A redelivery uses the same message key.
A new command requires a new message.
An edited message with changed intent returns a payload-mismatch conflict.

Hash the canonical user command before resolving server-derived revision or default expiry. Check the saved result before deriving these values on a redelivery. This prevents an expiry-command replay from using a newer revision. Keep responses within the configured Chat interaction deadline. No proactive notifications or reminders are planned.

## Acceptance-test scenarios

These scenarios define repository acceptance and staged rollout gates.
Local tests use disposable MySQL and signed identity fixtures, then exercise the registered HTTP and Chat adapters.
Concurrency tests must use independent DB connections and barriers to prove the race outcome, not just sequential behavior.

| ID | Scenario | Action | Expected result |
|---|---|---|---|
| S01 | App sandbox is independent | Claim app A in sandbox; query sibling app B and app A in prod | A/sandbox is taken; sibling and prod remain free. |
| S02 | Group claim covers present and future apps | Claim whole group, register another app, query/acquire it as another email | Group and every app are blocked in selected environments, including the later app. |
| S03 | Both environments are atomic | Place a different-owner prod conflict, then acquire sandbox+prod | One busy result; no sandbox-only claim or partial environment row is committed. |
| S04 | App/group overlap is symmetric | Acquire app then group from another email; repeat in reverse order | Each second acquisition is busy in either order. |
| S05 | Same-owner claims overlap independently | Same email takes overlapping app and group manual claims, then releases one ID | Both acquisitions succeed; only the selected claim becomes inactive. |
| S06 | Same owner is allowed, another owner is not | Create an active claim, request it with the same verified email and a different email | Query says taken but allowed for the same owner; a new same-owner CI claim succeeds; other owner gets busy. |
| S07 | Concurrent same-owner CI jobs | Barrier-start two requests with distinct job/request IDs and same verified email | Both succeed with separate claim IDs; neither is an execution mutex. |
| S08 | Different-owner acquisition race | Barrier-start two conflicting owners in the same group/environment | Exactly one acquires; the other receives busy with the committed conflicting claim. |
| S09 | Registration and claim race | Race group/app registration and app/group acquisitions using separate DB connections | Group/app rows are unique and parent-lock ordering prevents a missed group conflict or duplicate registration. |
| S10 | Custom-expiry equality boundary | Acquire with a known expiry and query just before, at, and after it | Active just before; free exactly at and after expiry. |
| S11 | Berlin default crosses DST | Acquire manually and through CI around the 2026 spring and fall transitions | Each uses next calendar day at local noon with correct Berlin offset, not `now+24h`; results agree. |
| S12 | Invalid expiry has no writes | Submit malformed, missing-offset REST, nonexistent/ambiguous Chat local, and nonfuture expiry | Each is rejected; no group, app, claim, history, or request result is committed. |
| S13 | Successful unknown-target acquisition registers catalog | Acquire a valid new group/app target | Acquisition succeeds and the group/app appear in read-only catalog queries. |
| S14 | Busy unknown-target acquisition still registers catalog | Create a conflicting group claim, then request a new app under that group from another owner | Busy is returned; group/app registration commits; no claim is added. |
| S15 | Any team member manages any claim | A different authenticated member changes expiry and releases both a manual and a CI claim through REST and Chat | All commands succeed without an administrator or originating-job permission. History records the actor. Owner and scope remain unchanged. |
| S16 | Expiry edit preserves history | Change an active claim's expiry and query before and after edit time | Earlier query returns old expiry; later query returns new expiry and revision. |
| S17 | Expired claim cannot be revived | Edit expiry after its prior expiry, then acquire anew | Edit is rejected as inactive; a new acquisition creates a new claim ID. |
| S18 | Retention preserves needed history and purges terminal history | Query older than 90 days. Prune a long-running claim spanning the cutoff and a terminal claim older than cutoff. | Old query is unavailable. The spanning version remains queryable. Unreferenced terminal claim, environment, and version rows are removed. |
| S19 | Future result is a projection | Query a future time, then acquire another claim now | Earlier projection is marked projected/not a reservation; it did not block the new acquisition. |
| S20 | Unknown read is side-effect free | Query an unknown app in a known group and an unknown group | Both report `known:false`; group claims are returned as inherited where applicable; no catalog row is added. |
| S21 | Lost-response replay is stable | Acquire once, discard response, repeat same actor/request ID/payload | Original claim ID and expiry are returned; no second claim is created. |
| S22 | Request ID payload mismatch is rejected | Reuse an actor's request ID with changed payload and with another group | Both return 409; cross-group loser rolls back provisional registration. |
| S23 | Inactive success replay blocks CI | Acquire, release or expire the claim, then replay the original successful request | Original outcome is returned with `activeNow:false`; CI caller does not deploy. |
| S24 | Rollback and ambiguous commit outcomes stay distinct | Inject a precommit failure, then separately drop the connection during COMMIT acknowledgement | Precommit failure leaves no writes. An ambiguous commit may have committed every row or none, never a partial claim. Return an error, not false or unverified success. Same-ID replay resolves the saved outcome. |
| S25 | Invalid GitLab identity is denied before writes | Send invalid signature, issuer, audience, expired-token, and non-team email cases | Each is 401/403 as appropriate and creates no catalog, claim, or request-result row. No project-specific compliance role is required. |
| S26 | Body identity cannot override account ownership | Supply a forged owner field or use different Google/GitLab account emails to acquire an overlapping target | A forged owner is rejected. Different emails conflict on acquisition. All authenticated team members can still manage existing claims. |
| S27 | Invalid Chat request/user is denied | Test invalid Chat token/audience, a non-team user, and an event without user email | No mutation occurs and no fallback to the bot service account, display name, or typed owner occurs. |
| S28 | Duplicate Chat message mutates once | Redeliver one authenticated slash-command message with the same `message.name`, including an expiry change after its first execution | One mutation/history transition occurs. Both responses use the saved outcome. Changed intent on that message returns 409. |
| S29 | End-to-end registered-route smoke | Against disposable MySQL, exercise the registered HTTP route from a CI shell caller and a Chat test-space event; render the bot's actual busy/success response | Auth, persistence, CI branching, deduplication, and bot formatting work through the real adapters; cleanup leaves no shared production data. |
| S30 | CI distinguishes busy from failures | Return busy, then separately inject auth, network, and DB failures in the caller smoke | Busy exits nonzero without deployment; auth/network/storage failures are reported as failures, never parsed as busy or false. |
| S31 | Public clone and worktree bootstrap | From a clean clone and a separate worktree without private credentials, run `mise trust`, `mise install`, and `mise run setup` | Both use the same public module graph and setup successfully; no private replacement or worktree-specific setup is required. |
| S32 | Tool versions agree | Compare the Go directive, generator module versions, and Mise-pinned tool versions | `go.mod` Go version matches Mise; Mockery and oapi-codegen tool directives resolve to module versions matching Mise; local and CI resolve the same Goose CLI. |
| S33 | Generated drift is rejected | In an isolated checkout, change an OpenAPI or mock generator input without updating its outputs, then separately add an untracked generated output and run `generate-check` | Both forms of drift fail; a clean `go generate ./...` leaves generated outputs unchanged. |
| S34 | Hooks and secret scanning work | Install hooks from a clean clone; run `prek run --all-files` and `mise run secret-scan` on safe examples and a disposable canary-secret fixture | Hooks and scans run as configured, reject the canary, and do not require broad allowlists or real credentials. |
| S35 | Public dependency, config, and CI boundary | Inspect the Go dependency graph and sample config, then exercise pull-request CI metadata | Dependencies and configs use public sources and dummy values with no private replacements, nonpublic hosts, or committed secrets. CI uses public actions/runners with read-only permissions. Pull requests never publish or deploy. |
| S36 | Main push publishes the commit image | Run a successful `main` push, then separately fail a required check | Success publishes the full checked-out commit hash and `latest` tags on the same digest, with matching OCI revision/version labels. Failed checks cause no registry login or push. |
| S37 | Git-tag push publishes all aliases | Push `v1.2.3` for a known commit, including an annotated-tag case | The full commit hash, `v1.2.3`, and `latest` image tags resolve to the same pushed digest. OCI revision is the commit hash, not the tag object. OCI version retains `v1.2.3`. No extra aliases appear. |
| S38 | Pull requests cannot publish | Run same-repository and fork pull-request workflows | Checks run with read-only permissions. No publishing job, Docker Hub login, or registry-secret access occurs. |

## Implementation sequence

1. **Public setup and tooling.** Public modules, safe examples, pinned tools, generators, hooks, Docker, and CI are implemented. The MIT license is unchanged.
2. **Bootstrap and deployment contract.** Runtime initialization checks MySQL version, UTC sessions, and the six InnoDB tables. Goose applies explicit migrations. Authenticated routes share one SQLC runtime. Database shutdown follows HTTP drain.
3. **Schema and transactional core.** Six tables, permanent group locks, operation-time acquisition, atomic environments, conflict checks, and global idempotency are implemented. Independent-pool barrier tests cover the races and rollback.
4. **Lifecycle and temporal reads.** Release, revision-checked expiry changes, immutable versions, current/history/projection queries, and terminal-time retention are implemented. Expired claims cannot be revived.
5. **REST and CI identity.** Generated typed requests, read-only SQLH catalogs, signed GitLab identity, and one-shot shell callers are implemented. Registered-route tests use real MySQL and signed tokens.
6. **Chat adapter.** Service-token and human-user verification, allowed spaces, structured commands, formatting, and message idempotency are implemented. Signed Chat/MySQL tests cover duplicate delivery and cross-owner management.
7. **Staged rollout.** Configure the actual MySQL server, issuers, audiences, team domain, and Chat space before enabling consumers. Exercise a staged GitLab job and dedicated Chat space. Keep deployment protections, TLS, and database backups.

## Verification and rollout

Observed local verification:

- The full `mise run check` task passes, including formatting, vet, lint, unit and MySQL integration tests, generation, migration validation, secret scans, and native build.

- The native executable builds and `go vet ./...` passes.
- Auth and domain tests pass with the race detector.
- The MySQL scenario suite passes against disposable MySQL 8.0.42, including independent-connection barriers and actual lost-COMMIT-acknowledgement replay.
- Registered REST routes pass signed identity, catalog isolation, invalid-input, ownership, and actual CI shell acquire/release tests.
- Registered Chat routes pass signed event, any-member management, idempotency, busy/success reply, and projection tests.
- CI caller tests distinguish busy/inactive from auth, transport, storage, and response-shape failures.
- The actual `claimy:ci` image passes HTTPS-JWKS, REST, Chat, persisted MySQL history, health, and graceful-shutdown smoke.
- Goose migration validation, module/tool version alignment, clean generation, and redacted secret scans pass.
- Gitleaks rejects a disposable synthetic token canary.

The schema permits an inactive release result whose terminal retention boundary has already elapsed before the next daily prune.
Claim expiry has no lifetime cap. Result-retention timestamps saturate at MySQL's maximum `DATETIME(6)` without changing claim expiry.

Run `mise run check` for the full contributor and CI task set.
Run `mise exec -- prek run --all-files` for installed hooks.
Build `claimy:ci`, then set `CLAIMY_IMAGE=claimy:ci` for the real-image integration test.
Tests apply and reverse migrations only in disposable databases.
Never point concurrency or failure-injection tests at a shared database.

The local fixtures do not establish a live GitLab or Google Chat deployment.
Production and staged configuration must supply the actual trusted issuers, exact audiences, team domain, and allowed space names.
MySQL must use InnoDB, enforce checks, match the configured supported baseline, and provide UTC operation time.
The binary includes Berlin timezone data.
GitLab must supply the supported stable user, project, and job identity claims.
Chat must provide an authenticated human email and use the public endpoint audience.
All authenticated team members share the same claim-management permission.
Do not add administrator, owner-only, or compliance gates.

Before staged enablement, run a GitLab test job and dedicated Chat-space commands against the HTTPS deployment.
Verify acquired, busy, inactive replay, auth/storage failures, formatted replies, and event deduplication.
Capture only redacted request/result metadata. Never log bearer tokens.
No shared production database, Chat app, or deployment resource was changed by local tests.

Publishing acceptance S36-S38 also requires current workflow-run and registry evidence.
Successful main/tag pushes must preserve the full commit hash, exact release tag, `latest`, matching digest, and OCI labels.
Failed checks and pull requests must not execute registry login or publishing.
