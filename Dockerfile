FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm AS build

ARG TARGETARCH

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=${TARGETARCH}
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/claimy ./cmd/claimy && \
    go build -C /go/pkg/mod/github.com/pressly/goose/v3@v3.24.3 \
    -trimpath -ldflags="-s -w -X main.version=v3.24.3" \
    -tags='no_clickhouse no_libsql no_mssql no_postgres no_sqlite3 no_vertica no_ydb' \
    -o /out/goose ./cmd/goose

FROM scratch

WORKDIR /app

COPY --from=build /out/claimy /app/claimy
COPY --from=build /out/goose /app/goose
COPY --from=build /src/config.dist.yml /app/config.dist.yml
COPY --from=build /src/build/migrations/claimy /app/build/migrations/claimy
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 65532:65532
EXPOSE 8088
ENTRYPOINT ["/app/claimy"]
CMD ["serve"]
