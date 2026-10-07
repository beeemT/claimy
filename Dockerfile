FROM golang:1.27.0-bookworm AS build

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOOS=linux

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/claimy ./cmd/claimy

FROM scratch

WORKDIR /app

COPY --from=build /out/claimy /app/claimy
COPY --from=build /src/config.dist.yml /app/config.dist.yml
COPY --from=build /src/build/migrations/claimy /app/build/migrations/claimy
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 65532:65532
EXPOSE 8088
ENTRYPOINT ["/app/claimy"]
CMD ["serve"]
