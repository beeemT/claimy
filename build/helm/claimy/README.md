# Claimy Helm chart

This chart deploys the Claimy HTTP service and its pre-install/pre-upgrade
Goose migration Job. It owns one Deployment, one ClusterIP Service, one
ConfigMap, and the migration Job. MySQL, credentials, ingress, TLS
termination, NetworkPolicy, and any other network resource remain the
responsibility of the operator.

The chart is named `claimy-chart`. The Docker image is `beeemt/claimy`; the
chart is published separately as the OCI artifact `beeemt/claimy-chart` so
chart versions never overwrite image tags.

## Required configuration

The chart intentionally has no usable identity-provider defaults. Set all of
the blank values in `values.yaml`, especially:

- `database.host`, `database.port`, `database.name`, and `database.user`;
- `database.existingSecret` and `database.passwordKey`;
- `config.claimy.auth.team_domain`;
- `config.claimy.auth.rest`, `.gitlab`, and `.chat` issuer, audience, and
  `jwks_url` values; and
- `config.claimy.chat.app_identity`, `allowed_spaces`, and `deadline`.

The `config` value uses Claimy's existing `config.dist.yml` shape. Configure
`config.claimy.auth.*` and `config.claimy.chat.*` in that map; do not create a
second top-level auth configuration. See the
[authentication guide](../../../docs/authentication.md) for provider setup and
accepted configuration fields. Browser CLI login is disabled by default at
`config.claimy.auth.cli.enabled: false`. Enable it only after supplying the
public client metadata required by Claimy.

The chart overlays the following fields and does not allow the runtime YAML
to disagree with chart values:

- `httpserver.default.port` comes from `service.port` (8088 by default);
- `httpserver.default.timeout.drain` and `.timeout.shutdown` are fixed at
  5s and 60s, respectively, while `kernel.kill_timeout` is fixed at 70s;
- `sqlc.default.uri.host`, `port`, `user`, and `database` come from
  `database.*`;
- `sqlc.default.parameters` comes from `database.parameters`; required values
  are `loc: UTC`, `time_zone: "'+00:00'"`, and `parseTime: "true"`. The schema
  prevents overriding these because Claimy requires UTC database sessions and
  Goose needs native MySQL time scanning; and
- `sqlc.default.migrations.enabled` is always `false` because the hook owns
  migrations.

The database password is never put in a values file or ConfigMap. In the
server Pod, `CLAIMY_DATABASE_PASSWORD` reads the password from the pre-existing
Secret. Claimy's runtime applies this raw value as the database credential
after loading the ordinary settings, so brace characters are not interpolated.
The Secret value is never rendered into the ConfigMap.

The migration Job reads the same Secret into `MYSQL_PASSWORD` before
constructing `GOOSE_DBSTRING` by Kubernetes environment expansion. The
password is not placed in process arguments and no shell is used. DSN query
parameter keys and values in `database.parameters` are URL-encoded for Goose.
Database host, user, and name are constrained by the values schema to ordinary
DSN-safe forms; use a DNS name rather than embedding protocol or credentials
in `database.host`.

## Create the database Secret

Create the Secret separately, before `helm install` or any upgrade that runs a
migration hook. Prefer a private file or stdin instead of placing the password
in shell history or a values file:

```sh
umask 077
printf '%s' "$CLAIMY_DB_PASSWORD" \
  | kubectl create secret generic claimy-db \
      --from-file=password=/dev/stdin
unset CLAIMY_DB_PASSWORD
```

Or write a file readable only by the current user and remove it after creation:

```sh
umask 077
printf '%s' "$CLAIMY_DB_PASSWORD" > claimy-db-password
kubectl create secret generic claimy-db \
  --from-file=password=./claimy-db-password
rm -f claimy-db-password
unset CLAIMY_DB_PASSWORD
```

Set `database.existingSecret: claimy-db` and
`database.passwordKey: password`. The chart does not create, update, or delete
this Secret. The Secret must be in the same Kubernetes namespace as the Helm
release; pass `--namespace` consistently to Secret creation and Helm commands.
Changing the Secret does not update running Pods; roll out/restart the
Deployment after password rotation so all replicas use the new credential.

## Install, upgrade, and rollback

Set a non-empty `image.tag` in the values supplied to every install or
upgrade. The source chart intentionally does not select a fake `0.0.0` image;
the selected image must be a built or published Claimy image that contains
both `/app/claimy` and `/app/goose`. For a stable Git tag `v1.2.3`, set
`image.tag: v1.2.3`; the corresponding OCI chart version is `1.2.3`
(without the leading `v`). The chart OCI artifact is available only after the
stable-tag image, package, and chart publication jobs all succeed.

From a checkout:

```sh
helm install claimy ./build/helm/claimy -f claimy-values.yaml \
  --wait --timeout 10m
helm upgrade claimy ./build/helm/claimy -f claimy-values.yaml \
  --wait --timeout 10m
helm rollback claimy 1 --wait --timeout 10m
```

From the published chart OCI repository:

```sh
helm install claimy \
  oci://registry-1.docker.io/beeemt/claimy-chart \
  --version 0.1.0 \
  -f claimy-values.yaml \
  --wait --timeout 10m
```

Pin `image.digest` or an immutable `image.tag` for production. The chart
version and OCI repository are independent from the `beeemt/claimy` image
repository. Helm rollback changes the workload and ConfigMap back to an older
release; it does **not** reverse SQL migrations already applied to MySQL.

The migration Job runs as a Helm `pre-install,pre-upgrade` hook using the same
MySQL settings and Secret as the server. Successful hook Jobs are deleted;
failed Jobs are retained for diagnosis. A failed migration blocks the install
or upgrade and no new application rollout is considered successful. Set
`migrations.enabled: false` only when migrations are applied by a separately
controlled process.

Migrations can acquire MySQL metadata or table locks. Run only one install or
upgrade against a database at a time, and schedule schema-changing upgrades
when concurrent application deploys or other DDL are quiesced. The application
image must connect to MySQL 8.0.16 or newer. MariaDB is not supported, and the
Claimy schema requires InnoDB with the session time zone set to `+00:00`.

## Runtime health and capacity

The named Service and container port are `http` and default to 8088. The
Service is always `ClusterIP`; add an ingress, gateway, or external TLS edge
in the deployment environment if one is needed.

- `/health` is an unauthenticated liveness endpoint. It is suitable for
  deciding whether the HTTP process is alive and does not replace database
  readiness.
- `/ready` is an unauthenticated readiness endpoint. It performs a database
  `PingContext` bounded to two seconds, so a database outage removes the Pod
  from Service endpoints without pretending the application is ready.

The default container is non-root UID/GID 65532, uses a read-only root
filesystem, drops all Linux capabilities, disables privilege escalation, and
does not receive a service-account token. The default runtime configuration
allows five seconds to drain new traffic, 60 seconds to finish active HTTP
requests, and a 70-second kernel shutdown budget inside the 75-second Pod
termination grace period. Keep `terminationGracePeriodSeconds` at or above
75 seconds when changing those settings.

`resources`, node placement, tolerations, affinity, and topology spread are
ordinary Pod controls. SQL pools are **per replica**, not global: the default
`config.sqlc.default.max_open_connections` is 10 and
`max_idle_connections` is 2. Multiply the open-connection setting by the
maximum number of running replicas (and leave headroom for the migration and
other database clients) before choosing MySQL limits.

## IdP CA certificates and other mounts

The scratch image includes the normal system CA bundle. For an identity
provider using a private CA, use the conventional `extraVolumes`,
`extraVolumeMounts`, and `extraEnv` values. For example, mount a ConfigMap or
Secret containing a complete PEM bundle and point the Go TLS client at it:

```yaml
extraVolumes:
  - name: idp-ca
    secret:
      secretName: claimy-idp-ca
extraVolumeMounts:
  - name: idp-ca
    mountPath: /etc/claimy/ca
    readOnly: true
extraEnv:
  - name: SSL_CERT_FILE
    value: /etc/claimy/ca/ca-certificates.crt
```

The same optional mounts are available to the migration Job. Do not place
passwords, OAuth client secrets, or bearer tokens in `config` or `extraEnv`.
