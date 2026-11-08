FROM golang:1.27.3-alpine AS gobuild
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/flux-agent ./cmd/flux-agent

# Build an isolated Python environment; installation tools stay out of runtime.
FROM ubuntu:24.04 AS pythonbuild
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-venv ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY requirements-runtime.txt /tmp/requirements-runtime.txt
RUN python3 -m venv /opt/patroni \
    && /opt/patroni/bin/python -m pip install --no-cache-dir --upgrade pip==26.2.0 \
    && /opt/patroni/bin/python -m pip install --no-cache-dir --require-hashes -r /tmp/requirements-runtime.txt \
    && /opt/patroni/bin/python -m pip check \
    && /opt/patroni/bin/python -m pip uninstall -y pip setuptools wheel

FROM ubuntu:24.04

ARG POSTGRES_MAJOR=14
ARG VERSION=dev

ENV DEBIAN_FRONTEND=noninteractive
ENV POSTGRES_MAJOR=${POSTGRES_MAJOR}
ENV PATH="/opt/patroni/bin:${PATH}"

LABEL org.opencontainers.image.version="${VERSION}" \
      runonflux.postgres.major="${POSTGRES_MAJOR}"

# Install system dependencies, then PostgreSQL from PGDG so 14 and 15 share one Dockerfile.
RUN apt-get update && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
    curl \
    jq \
    python3 \
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
    && echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        postgresql-${POSTGRES_MAJOR} \
        postgresql-client-${POSTGRES_MAJOR} \
        postgresql-${POSTGRES_MAJOR}-pgvector \
    && rm -rf /var/lib/apt/lists/*

COPY --from=pythonbuild /opt/patroni /opt/patroni

# Create necessary directories
RUN mkdir -p /etc/patroni /app /var/log/supervisor /var/lib/postgresql/data /var/lib/postgresql/backups /etc/ssl/cluster/{ca,etcd,postgres,patroni}

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

# etcd 3.5.35 with the x/net security backport; see bin/ETCD_BUILD.md.
COPY bin/etcd bin/etcdctl bin/SHA256SUMS /usr/local/bin/
RUN cd /usr/local/bin && sha256sum -c SHA256SUMS && rm SHA256SUMS

# Copy Go binary
COPY --from=gobuild /out/flux-agent /app/flux-agent

# Make scripts executable
RUN chmod +x /app/diagnose.sh /app/generate-certs.sh /app/post_bootstrap.sh /app/flux-agent

# Set working directory
WORKDIR /app

# Expose ports
# 5432 = postgres direct, 8008 = patroni REST, 2379/2380 = etcd, 5433 = primary-routing proxy
EXPOSE 5432 5433 8008 2379 2380

# Runtime volumes are mounted after the image is built and may hide directories
# created above or replace them with root-owned mount points. Recreate the
# Supervisor log directory and make the configured backup directory writable by
# the postgres user before init or Supervisor starts.
CMD ["/bin/bash", "-c", "mkdir -p /var/log/supervisor && install -d -o postgres -g postgres -m 0700 \"${BACKUP_DIR:-/var/lib/postgresql/backups}\" && /app/flux-agent init && exec supervisord -n"]
