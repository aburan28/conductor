# Build from the repository root:
# docker buildx build --platform linux/amd64 -f deploy/aws/ha/Dockerfile -t conductor-ha --load .
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/conductord ./cmd/conductord \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/conductor-healthcheck ./cmd/conductor-healthcheck

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS certificates
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
       https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
       -o /etc/ssl/certs/rds-global-bundle.pem

# Only static binaries and public certificate bundles enter the runtime image. curl,
# its native compression/TLS dependencies, the shell, and package tools stay in builders.
FROM scratch
COPY --from=build /out/conductord /usr/local/bin/conductord
COPY --from=build /out/conductor-healthcheck /usr/local/bin/conductor-healthcheck
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certificates /etc/ssl/certs/rds-global-bundle.pem /etc/ssl/certs/rds-global-bundle.pem
USER 10001:10001
WORKDIR /var/lib/conductor
# pgx reads the PG* variables and secrets directly, without shell/URL interpolation.
ENV DATABASE_URL="connect_timeout=5" \
    HOME=/var/lib/conductor \
    CONDUCTOR_STATE_DIR=/var/lib/conductor \
    PGSSLMODE=verify-full \
    PGSSLROOTCERT=/etc/ssl/certs/rds-global-bundle.pem
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/conductord"]
