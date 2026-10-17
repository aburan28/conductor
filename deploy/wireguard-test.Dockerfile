# syntax=docker/dockerfile:1
FROM debian:bookworm-slim
RUN --mount=type=secret,id=proxy_ca,required=true \
    apt-get -o Acquire::https::CaInfo=/run/secrets/proxy_ca update \
    && apt-get -o Acquire::https::CaInfo=/run/secrets/proxy_ca install -y --no-install-recommends wireguard-tools wireguard-go iproute2 iputils-ping \
    && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
