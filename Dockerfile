FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Install system dependencies
RUN apt-get update && apt-get install -y \
    postgresql-14 \
    postgresql-client-14 \
    etcd-server \
    etcd-client \
    curl \
    jq \
    python3 \
    python3-pip \
    supervisor \
    sudo \
    wget \
    gnupg \
    ca-certificates \
    net-tools \
    procps \
    && rm -rf /var/lib/apt/lists/*

# Install Python packages
RUN pip3 install patroni[etcd] psycopg2-binary

# Create necessary directories
RUN mkdir -p /etc/patroni /app /var/log/supervisor /var/lib/postgresql/data

# Create postgres user and set permissions
RUN chown -R postgres:postgres /var/lib/postgresql
RUN chmod 700 /var/lib/postgresql/data

# Copy configuration templates and scripts
COPY entrypoint.sh /app/entrypoint.sh
COPY patroni.yml.tpl /app/patroni.yml.tpl
COPY update-cluster.sh /app/update-cluster.sh
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY diagnose.sh /app/diagnose.sh
COPY fix-cluster.sh /app/fix-cluster.sh
COPY VERSION /app/VERSION

# Make scripts executable
RUN chmod +x /app/entrypoint.sh /app/update-cluster.sh /app/diagnose.sh /app/fix-cluster.sh

# Set working directory
WORKDIR /app

# Expose ports
EXPOSE 5432 8008 2379 2380

# Run entrypoint script then start supervisord
CMD ["/bin/bash", "-c", "/app/entrypoint.sh && supervisord -n"]