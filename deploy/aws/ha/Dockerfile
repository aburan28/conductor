# Build from the repository root:
# docker buildx build --platform linux/amd64 -f deploy/aws/ha/Dockerfile -t conductor-ha --load .
FROM golang:1.26.10-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/conductord ./cmd/conductord

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
       https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
       -o /etc/ssl/certs/rds-global-bundle.pem \
    && useradd --uid 10001 --create-home --home-dir /var/lib/conductor conductor
COPY --from=build /out/conductord /usr/local/bin/conductord
USER 10001:10001
WORKDIR /var/lib/conductor
# pgx reads the PG* variables and secrets directly, without shell/URL interpolation.
ENV DATABASE_URL="connect_timeout=5" \
    CONDUCTOR_STATE_DIR=/var/lib/conductor \
    PGSSLMODE=verify-full \
    PGSSLROOTCERT=/etc/ssl/certs/rds-global-bundle.pem
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/conductord"]
