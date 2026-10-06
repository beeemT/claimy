# Claimy

Claimy is a Go/Gosoline HTTP service for the environment-claim design in [the plan](docs/environment-claim-plan.md).

The current executable exposes the framework health endpoint. Claim acquisition, history, GitLab identity, and Google Chat commands are not implemented yet.

## Development

Install [Mise](https://mise.jdx.dev/) and run these commands from the repository root:

```sh
mise trust
mise install
mise run setup
mise run check
mise run run
```

The setup task downloads public Go modules and installs Prek Git hooks.
The same setup works in an ordinary clone or a Git worktree.
The `worktree-setup` task also runs setup for worktree tools.

Claimy loads `config.dist.yml` from its working directory and listens on port 8088.
Check the actual framework health endpoint:

```sh
curl --fail http://localhost:8088/health
```

A healthy process returns HTTP 200 and `{}`.
The framework handles SIGINT and SIGTERM shutdown.
The binary includes timezone data, including `Europe/Berlin`.

Use environment variables to override configuration without committing local values:

```sh
HTTPSERVER_DEFAULT_PORT=9090 mise run run
```

## Repository checks

Mise pins Go 1.27.0, gofumpt, stock golangci-lint, Prek, and Gitleaks.

| Task | Purpose |
|---|---|
| `mise run setup` | Download dependencies and install Git hooks |
| `mise run version-check` | Compare the Go version in `go.mod` and `mise.toml` |
| `mise run fmt` | Format Go source |
| `mise run fmt-check` | Reject unformatted Go source |
| `mise run vet` | Run Go vet |
| `mise run lint` | Run the configured built-in Go linters |
| `mise run test` | Run the available Go tests |
| `mise run secret-scan` | Scan Git history and working files with redacted Gitleaks output |
| `mise run build` | Build `build/bin/claimy` |
| `mise run check` | Run all checks and build the executable |

Prek checks merge markers, TOML, YAML, line endings, whitespace, Go formatting, Go lint, and staged secrets.
Run hooks manually with `mise exec -- prek run --all-files`.

Keep private configuration, credentials, generated indexes, and build output out of commits.
Generation, database migrations, and MySQL integration tasks will accompany their real source inputs and tests.
They are not empty tasks in the current check command.

## Container

Docker builds a static Linux executable and packages it with safe configuration and a CA bundle.
The runtime uses a nonroot user and contains no shell or Go build tools.

```sh
docker build --tag claimy:local .
docker run --rm --name claimy --publish 127.0.0.1:8089:8088 claimy:local
```

From another terminal:

```sh
curl --fail http://localhost:8089/health
```

Local builds use the host's Docker architecture.
GitHub Actions currently publishes `linux/amd64` images to `docker.io/beeemt/claimy`.
A Docker Hub login is required if the repository is private.
On an ARM host, select the published architecture explicitly:

```sh
docker run --rm --platform linux/amd64 --publish 127.0.0.1:8089:8088 docker.io/beeemt/claimy:latest
```

## Continuous integration and publishing

GitHub Actions runs repository checks, builds the image, starts a container, and requires HTTP 200 from `/health`.
The workflow uses public actions pinned to commit hashes and read-only repository permissions.

Pull requests run checks without Docker Hub credentials or image publishing.
After checks succeed, pushes to `main` and Git tags publish one image with these aliases:

- The full checked-out commit hash.
- `latest`.
- The exact Git tag, on tag pushes only.

All aliases from a publishing job refer to the same image digest.
OCI labels record the source commit and release version.
`latest` moves with each successful publication.
A release Git tag must be a valid Docker image tag name.
The workflow does not deploy the application.

Configure the Actions secret `DOCKERHUB_TOKEN` with a Docker Hub personal access token that has **Read & Write** permissions.
Do not grant Delete access.
The login username uses `DOCKERHUB_USERNAME` from an Actions secret or repository variable, with `beeemt` as its default.
The destination remains `docker.io/beeemt/claimy`.
Never put registry credentials in build arguments, image layers, or source files.

## License

[MIT](LICENSE).
