# Local Testing How-To

## Overview

The local test setup spins up **4 containers** using Docker Compose:

| Container | Role | Description |
|-----------|------|-------------|
| `mock-flux-api` | Mock API | nginx serving static JSON to simulate the Flux API |
| `postgres-node1` | PG cluster node | PostgreSQL + Patroni + etcd on 172.20.0.10 |
| `postgres-node2` | PG cluster node | PostgreSQL + Patroni + etcd on 172.20.0.11 |
| `postgres-node3` | PG cluster node | PostgreSQL + Patroni + etcd on 172.20.0.12 |

## Prerequisites

- Docker with Compose v2 (`docker compose version`)
- A `.env` file in the project root (see below)

## .env File

Create `.env` in the project root with at minimum:

```env
APP_NAME=myapp-postgres

HOST_POSTGRES_PORT=5432
HOST_PATRONI_API_PORT=8008
HOST_ETCD_CLIENT_PORT=2379
HOST_ETCD_PEER_PORT=2380

POSTGRES_PORT=5432
PATRONI_API_PORT=8008
ETCD_CLIENT_PORT=2379
ETCD_PEER_PORT=2380

POSTGRES_SUPERUSER_PASSWORD=supersecretpassword
POSTGRES_REPLICATION_PASSWORD=supersecretreplication

POSTGRES_DB=postgres
POSTGRES_USER=postgres

SSL_ENABLED=false
SSL_PASSPHRASE=change-this-to-secure-deterministic-passphrase
SSL_CERT_VALIDITY_DAYS=3650
```

> **Note**: `docker-compose.yml` overrides `SSL_ENABLED=true` for all nodes, so SSL is active in local tests regardless of the `.env` value.

## Starting the Cluster

```bash
docker compose up -d --build
```

This builds the image and starts all 4 containers. Cluster formation takes ~60–90 seconds.

## Port Mapping

| Service | Host Port | Description |
|---------|-----------|-------------|
| Mock Flux API | `8080` | `http://localhost:8080` |
| Node 1 PostgreSQL | `5432` | Primary or replica |
| Node 2 PostgreSQL | `5433` | Primary or replica |
| Node 3 PostgreSQL | `5434` | Primary or replica |
| Node 1 Patroni API | `8008` | `https://localhost:8008` |
| Node 2 Patroni API | `8009` | `https://localhost:8009` |
| Node 3 Patroni API | `8010` | `https://localhost:8010` |
| Node 1 etcd client | `2379` | |
| Node 2 etcd client | `2381` | |
| Node 3 etcd client | `2383` | |

## Monitoring Containers

### Check which containers are running

```bash
docker compose ps
```

### View cluster status (who is the leader)

```bash
# Replace 8008/8009/8010 with any node's Patroni port
curl -sk https://localhost:8009/cluster | python3 -m json.tool
```

Output shows all members, their roles (`leader` or `replica`), and replication lag.

### Check a single node's role

```bash
curl -sk https://localhost:8008/   # node1
curl -sk https://localhost:8009/   # node2
curl -sk https://localhost:8010/   # node3
```

The `"role"` field will be `"primary"` or `"replica"`.

### Stream logs for a specific container

```bash
docker compose logs -f postgres-node1
docker compose logs -f postgres-node2
docker compose logs -f postgres-node3
docker compose logs -f mock-api
```

### Read internal log files inside a container

```bash
docker exec flux-pg-cluster-postgres-node1-1 tail -f /var/log/supervisor/patroni.out.log
docker exec flux-pg-cluster-postgres-node1-1 tail -f /var/log/supervisor/etcd.out.log
docker exec flux-pg-cluster-postgres-node1-1 tail -f /var/log/supervisor/updater.out.log
```

## Connecting to PostgreSQL

Find the primary node first (see cluster status above), then connect through its port.

```bash
# Connect to node2 (if it is the primary, port 5433)
docker exec -e PGPASSWORD=supersecretpassword flux-pg-cluster-postgres-node2-1 \
  psql -h 172.20.0.11 -U postgres -c "SELECT pg_is_in_recovery();"
# f = primary, t = replica
```

From the host (requires `psql` installed):

```bash
PGPASSWORD=supersecretpassword psql -h localhost -p 5433 -U postgres
```

## Stopping the Cluster

```bash
# Stop containers, keep volumes
docker compose down

# Stop and delete all data volumes (full reset)
docker compose down -v
```
