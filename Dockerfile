FROM golang:1.22 AS gobuild
WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/flux-agent ./cmd/flux-agent

FROM ubuntu:22.04

ARG POSTGRES_MAJOR=14
ARG VERSION=dev

ENV DEBIAN_FRONTEND=noninteractive
ENV POSTGRES_MAJOR=${POSTGRES_MAJOR}

LABEL org.opencontainers.image.version="${VERSION}" \
      runonflux.postgres.major="${POSTGRES_MAJOR}"

# Install system dependencies, then PostgreSQL from PGDG so 14 and 15 share one Dockerfile.
RUN apt-get update && apt-get install -y \
    curl \
    jq \
    python3 \
    python3-pip \
    supervisor \
    sudo \
    wget \
    gnupg \
    ca-certificates \
    lsb-release \
    net-tools \
    procps \
    openssl \
    xxd \
    vim-common \
    gnutls-bin \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        | gpg --dearmor -o /usr/share/keyrings/postgresql-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update \
    && apt-get install -y \
        postgresql-${POSTGRES_MAJOR} \
        postgresql-client-${POSTGRES_MAJOR} \
    && rm -rf /var/lib/apt/lists/*

# Install Python packages
RUN pip3 install 'patroni[etcd3]' psycopg2-binary cryptography

# Create necessary directories
RUN mkdir -p /etc/patroni /app /var/log/supervisor /var/lib/postgresql/data /etc/ssl/cluster/{ca,etcd,postgres,patroni}

# Create postgres user and set permissions
RUN chown -R postgres:postgres /var/lib/postgresql
RUN chmod 700 /var/lib/postgresql/data

# Copy configuration templates and scripts
# Legacy scripts retained for: certs generation (called from Go), diagnostics.
COPY patroni.yml.tpl /app/patroni.yml.tpl
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY post_bootstrap.sh /app/post_bootstrap.sh
COPY diagnose.sh /app/diagnose.sh
COPY generate-certs.sh /app/generate-certs.sh
COPY VERSION /app/VERSION

# Copy etcd 3.5 binaries (pre-downloaded to avoid build-time internet dependency)
COPY bin/etcd bin/etcdctl /usr/local/bin/

# Copy Go binary
COPY --from=gobuild /out/flux-agent /app/flux-agent

# Make scripts executable
RUN chmod +x /app/diagnose.sh /app/generate-certs.sh /app/post_bootstrap.sh /app/flux-agent

# Set working directory
WORKDIR /app

# Expose ports
# 5432 = postgres direct, 8008 = patroni REST, 2379/2380 = etcd, 5433 = primary-routing proxy
EXPOSE 5432 5433 8008 2379 2380

# Run init then start supervisord
CMD ["/bin/bash", "-c", "/app/flux-agent init && supervisord -n"]
