# Flux PostgreSQL Cluster
![Version](https://img.shields.io/badge/version-1.1.2-blue.svg)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14-blue.svg)
![Patroni](https://img.shields.io/badge/Patroni-latest-green.svg)
![Docker](https://img.shields.io/badge/Docker-required-blue.svg)

This project creates a self-configuring, highly-available PostgreSQL cluster that dynamically discovers its members through the Flux API. The cluster uses Patroni for PostgreSQL high availability, etcd for distributed coordination, and automatically adapts to nodes being added or removed from the environment.

## Prerequisites

- Docker
- Docker Compose
- Access to Flux network for API calls

## Quick Start

### Production Deployment on Flux Network

#### Architecture Overview

```
   ┌───────────────────┐       ┌───────────────────┐       ┌───────────────────┐
   │      Node 1       │       │      Node 2       │       │       Node 3      │
   │  ┌─────────────┐  │       │  ┌─────────────┐  │       │  ┌─────────────┐  │
   │  │  Your App   │  │       │  │  Your App   │  │       │  │  Your App   │  │
   │  │ (Component) │  │       │  │ (Component) │  │       │  │ (Component) │  │
   │  └──────┬──────┘  │       │  └──────┬──────┘  │       │  └──────┬──────┘  │
   │         │ :5433   │       │         │ :5433   │       │         │ :5433   │
   │  ┌──────▼──────┐  │       │  ┌──────▼──────┐  │       │  ┌──────▼──────┐  │
   │  │   Proxy     │  │       │  │   Proxy     │  │       │  │   Proxy     │  │
   │  │(primary-    │  │       │  │(primary-    │  │       │  │(primary-    │  │
   │  │  routing)   │  │       │  │  routing)   │  │       │  │  routing)   │  │
   │  └──────┬──────┘  │       │  └──────┬──────┘  │       │  └──────┬──────┘  │
   │         │         │       │         │         │       │         │         │
   │  ┌──────▼──────┐  │       │  ┌──────▼──────┐  │       │  ┌──────▼──────┐  │
   │  │ PostgreSQL  │  │       │  │ PostgreSQL  │  │       │  │ PostgreSQL  │  │
   │  │Patroni+etcd │  │       │  │Patroni+etcd │  │       │  │Patroni+etcd │  │
   │  │   PRIMARY   │◄─┼───────┼─►│  SECONDARY  │◄─┼───────┼─►│  SECONDARY  │  │
   │  │(Read+Write) │  │       │  │ (Read-Only) │  │       │  │ (Read-Only) │  │
   │  └─────────────┘  │       │  └─────────────┘  │       │  └─────────────┘  │
   └───────────────────┘       └───────────────────┘       └───────────────────┘
            │                            │                           │
            └────────────────────────────┼───────────────────────────┘
                            Replication via Public Internet
Key Points:
• Each application connects to its local proxy on port 5433
• The proxy polls Patroni to discover the current primary and forwards all connections there
• After a failover the proxy automatically reroutes new connections to the new primary
• PostgreSQL instances replicate data across nodes via public internet
• Only PRIMARY accepts writes; SECONDARY nodes are read-only
```

1. **Deploy on Flux**:
  - Log in to home.runonflux.io and navigate to Applications > Register New App.
  - Add a component for PostgreSQL.
  - Use the official Docker image: `runonflux/flux-pg-cluster:latest`.
  - Set the Container Data for the component to `/var/lib/postgresql/data`.
  - Add these ports to the `Cont. Ports` field: `[5432,5433,8008,2379,2380]`.
  - Using the `Ports` field, map those ports to new ones, for example: `[15432,15433,18008,12379,12380]`.
  - For the `Domains` field, add this: `["","","","",""]`.
  - Use the following sample to set the environment variables for the PostgreSQL component:

   ```json
   [
      "HOST_POSTGRES_PORT=15432",
      "HOST_PATRONI_API_PORT=18008",
      "HOST_ETCD_CLIENT_PORT=12379",
      "HOST_ETCD_PEER_PORT=12380",
      "POSTGRES_SUPERUSER_PASSWORD=your-super-secret-password",
      "POSTGRES_REPLICATION_PASSWORD=your-replication-password",
      "POSTGRES_DB=your-app-database",
      "SSL_ENABLED=true",
      "SSL_PASSPHRASE=your-ssl-passphrase"
   ]
   ```
    

2. **Connect from other Flux components**:
   ```bash
   # Recommended — connect via proxy (always routes to current primary):
   postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5433/[POSTGRES_DB]

   # With SSL (recommended):
   postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5433/[POSTGRES_DB]?sslmode=require

   # Direct connection to a specific node (bypasses proxy — use only for read replicas or diagnostics):
   postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/[POSTGRES_DB]
   ```

3. **Monitor your cluster**:
   - Access Patroni REST API: `https://your-app-name.app_{patroni_rest_api_port}.runonflux.io`


## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HOST_POSTGRES_PORT` | Host PostgreSQL port mapping | `5432` |
| `HOST_PATRONI_API_PORT` | Host Patroni REST API port mapping | `8008` |
| `HOST_ETCD_CLIENT_PORT` | Host etcd client port mapping | `2379` |
| `HOST_ETCD_PEER_PORT` | Host etcd peer communication port mapping | `2380` |
| `POSTGRES_PORT` | Internal PostgreSQL port | `5432` |
| `PATRONI_API_PORT` | Internal Patroni REST API port | `8008` |
| `ETCD_CLIENT_PORT` | Internal etcd client port | `2379` |
| `ETCD_PEER_PORT` | Internal etcd peer communication port | `2380` |
| `POSTGRES_SUPERUSER_PASSWORD` | PostgreSQL superuser password | Required |
| `POSTGRES_REPLICATION_PASSWORD` | PostgreSQL replication user password | Required |
| `POSTGRES_USER` | PostgreSQL username | `postgres` |
| `POSTGRES_DB` | Application database created on first bootstrap, owned by the `admin` role. Use `postgres` to skip creation of a separate database. | `postgres` |
| `SSL_ENABLED` | Enable SSL/TLS encryption for all services | `false` |
| `SSL_PASSPHRASE` | Deterministic passphrase for certificate generation | Required if SSL_ENABLED=true |
| `SSL_CERT_VALIDITY_DAYS` | Certificate validity period in days | `3650` |
| `ALLOW_NEW_CLUSTER_BOOTSTRAP` | Allow creating a new multi-member etcd cluster when no peers are reachable. Keep `false` during normal operations. | `false` |
| `ALLOW_ANY_NODE_BOOTSTRAP` | If `true`, bypass deterministic bootstrap-candidate restriction. Keep `false` for safety. | `false` |
| `ETCD_JOIN_MAX_RETRIES` | How many peer-join attempts are made before deciding bootstrap behavior | `12` |
| `ETCD_JOIN_RETRY_DELAY_SECONDS` | Delay between peer-join retries | `10` |
| `UPDATE_INTERVAL_SECONDS` | Update daemon reconciliation interval | `60` |
| `DESIRED_STATE_STABILITY_CYCLES` | API desired-state cycles required before membership removal/rewrite | `3` |
| `ETCD_UNAVAILABLE_RECOVERY_CYCLES` | Consecutive updater cycles with local etcd unavailable before peer-evidence recovery kicks in | `2` |
| `ETCD_UNAVAILABLE_COUNT_FILE` | Internal counter file used by updater for unavailable-etcd recovery state | `/tmp/etcd-unavailable-count` |
| `PATRONI_TTL` | Patroni DCS TTL (seconds) — how long a leader key is valid before another node may start an election | `30` |
| `PATRONI_LOOP_WAIT` | Patroni main loop interval (seconds) | `10` |
| `PATRONI_RETRY_TIMEOUT` | Patroni DCS operation retry timeout (seconds) | `30` |
| `PATRONI_MAX_LAG` | Maximum replication lag (bytes) a replica may have and still be eligible for leader election. Default 32 MB is generous enough to tolerate WAN jitter without excluding healthy replicas. | `33554432` |
| `PATRONI_MASTER_START_TIMEOUT` | Seconds Patroni waits for the primary to start before considering a failover | `300` |
| `PATRONI_MASTER_STOP_TIMEOUT` | Seconds Patroni waits for the primary to stop cleanly before forcibly terminating it | `300` |
| `PATRONI_USE_SLOTS` | Whether to use PostgreSQL replication slots. Disabled by default to prevent WAL accumulation when replicas disappear in high-churn Flux deployments. | `false` |
| `PATRONI_SYNCHRONOUS_MODE` | Enable synchronous replication — every commit waits for at least one replica to acknowledge before returning success. Eliminates data loss on failover but increases write latency and risks write-stall if all replicas go offline. See note below. | `false` |
| `PATRONI_SYNCHRONOUS_MODE_STRICT` | When `true`, the primary **blocks all writes** if no synchronous replica is available instead of silently falling back to async. Only meaningful when `PATRONI_SYNCHRONOUS_MODE=true`. | `false` |
| `PATRONI_SYNCHRONOUS_NODE_COUNT` | Number of synchronous replicas required to acknowledge a commit when `PATRONI_SYNCHRONOUS_MODE=true`. Maps to Patroni's `synchronous_node_count`. Has no effect when synchronous mode is disabled. | `1` |
| `PROXY_ENABLED` | Enable the TCP primary-routing proxy on port 5433. Set to `false` to disable (port 5433 will not be opened). | `true` |
| `PROXY_LISTEN_PORT` | Port the primary-routing proxy listens on inside the container | `5433` |
| `PROXY_HEALTH_INTERVAL_SECONDS` | How often (seconds) the proxy polls Patroni to discover the current primary | `3` |

### Split-Brain Prevention Controls

- Multi-member clusters no longer auto-bootstrap as new unless `ALLOW_NEW_CLUSTER_BOOTSTRAP=true`.
- By default, only one deterministic bootstrap candidate (lowest node name) may bootstrap a new cluster.
- Mismatch detection evaluates all reachable peers and only performs destructive self-heal when mismatch is the majority view.
- Membership removals and `ETCD_INITIAL_CLUSTER` rewrites are gated by desired-state stability to reduce churn-induced drift.

### Synchronous Replication

By default the cluster uses **asynchronous replication** — the primary commits writes without waiting for replicas. This maximises availability on Flux where nodes can be geographically distributed or intermittently unreachable.

Set `PATRONI_SYNCHRONOUS_MODE=true` to switch to **synchronous quorum replication**:

```json
"PATRONI_SYNCHRONOUS_MODE=true",
"PATRONI_SYNCHRONOUS_MODE_STRICT=false",
"PATRONI_SYNCHRONOUS_NODE_COUNT=1"
```

| `PATRONI_SYNCHRONOUS_MODE` | `PATRONI_SYNCHRONOUS_MODE_STRICT` | Behavior |
|---|---|---|
| `false` | — | Async replication (default) |
| `true` | `false` | Sync replication; silently falls back to async if no replica is available |
| `true` | `true` | Sync replication; **blocks all writes** if no replica is available |

| | Async (default) | Synchronous |
|---|---|---|
| **Write latency** | Low | Higher (one replica round-trip per commit) |
| **Data loss on failover** | Possible (last WAL records) | None |
| **Availability if replicas down** | Primary keeps writing | Primary **pauses writes** until a replica reconnects |

> **Warning**: With `PATRONI_SYNCHRONOUS_MODE=true`, if the number of available replicas drops below `PATRONI_MINIMUM_SYNCHRONOUS_REPLICAS`, the primary will stop accepting writes until a replica comes back online. Only enable this if your workload can tolerate that trade-off.

## How It Works

### Startup Process

1. **Discovery Phase**: Container calls `https://api.runonflux.io/apps/location/{APP_NAME}` to get all cluster member IPs
2. **Configuration Generation**: Creates Patroni and etcd configuration files using discovered IPs
3. **Service Startup**: Supervisord starts etcd, then Patroni, then the cluster update daemon

### Dynamic Membership

- **Background Process**: Continuously monitors Flux API (every 60 seconds)
- **Automatic Removal**: Removes nodes from etcd cluster when they're no longer in the API response
- **Self-Registration**: New nodes automatically join the cluster when they start up

### Service Management

The supervisord configuration manages four main processes:

- **etcd**: Distributed key-value store for cluster coordination
- **patroni**: PostgreSQL high availability manager
- **updater**: Background daemon that maintains cluster membership
- **proxy**: TCP primary-routing proxy on port 5433 (disable with `PROXY_ENABLED=false`)

### Access PostgreSQL

#### Primary-Routing Proxy (Recommended)

Each node runs a lightweight TCP proxy on **port 5433** that automatically routes all connections to the current Patroni primary. Your application does not need to know which node is the primary — just connect to any cluster node on port 5433 and writes will always land on the correct node, even after a failover.

```
App → any-node:5433 (proxy) → discovers primary via Patroni API → forwards to primary:5432
```

After a failover, the proxy detects the new primary within `PROXY_HEALTH_INTERVAL_SECONDS` (default 3 s) and routes new connections there automatically. In-flight connections are not interrupted.

#### Connection Strings

**Recommended (via proxy — always routes to primary):**
```
Host: flux{COMPONENT_NAME}_{APPNAME}
Port: 5433
Database: [POSTGRES_DB]
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5433/[POSTGRES_DB]

# With SSL enabled:
postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5433/[POSTGRES_DB]?sslmode=require
```

**Direct connection (bypasses proxy — use only for read replicas or diagnostics):**
```
Host: flux{COMPONENT_NAME}_{APPNAME}
Port: 5432
Database: [POSTGRES_DB]
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/[POSTGRES_DB]
```

**For external connections (from host machine or remote clients):**
```
# Via proxy (recommended for writes):
postgresql://postgres:[PASSWORD]@localhost:[HOST_PROXY_PORT]/[POSTGRES_DB]
# e.g. if mapped to 15433: postgresql://postgres:[PASSWORD]@localhost:15433/[POSTGRES_DB]

# Direct to a node (read-only or diagnostics):
postgresql://postgres:[PASSWORD]@localhost:[HOST_POSTGRES_PORT]/[POSTGRES_DB]
# e.g. if mapped to 15432: postgresql://postgres:[PASSWORD]@localhost:15432/[POSTGRES_DB]
```

**For local testing with multiple nodes** (using `docker-compose.yml` default mappings):
- Node 1 proxy (routes to primary): `postgresql://postgres:[PASSWORD]@localhost:5435/postgres`
- Node 1 direct: `postgresql://postgres:[PASSWORD]@localhost:5432/postgres`
- Node 2 direct: `postgresql://postgres:[PASSWORD]@localhost:5433/postgres`
- Node 3 direct: `postgresql://postgres:[PASSWORD]@localhost:5434/postgres`

**With SSL enabled, add `?sslmode=require` to any connection string above.**


### Patroni REST API

Access the Patroni REST API at `http://localhost:8008` to:
- View cluster status: `GET /cluster`
- Check member status: `GET /`
- Trigger failover: `POST /failover`

## Files Overview

- **Dockerfile**: Multi-stage build — Go binary compiled inside Docker, no local Go toolchain needed
- **docker-compose.yml**: Service definition with networking and volumes
- **patroni.yml.tpl**: Template for Patroni configuration
- **supervisord.conf**: Process management configuration (etcd, patroni, updater, proxy)
- **cmd/flux-agent/**: Go source for the agent binary (init, daemon, etcd bootstrap, proxy)

### Local Testing

For local development and testing, this repository includes a complete mock environment:

1. **Start local test cluster**:
   ```bash
   docker compose up -d --build
   ```

2. **Access local services**:
   - **Mock Flux API**: http://localhost:8080
   - **Primary-routing proxy** (node 1): `localhost:5435` → always connects to current primary
   - **PostgreSQL direct** (per-node):
     - Node 1: `localhost:5432`
     - Node 2: `localhost:5433`
     - Node 3: `localhost:5434`
   - **Patroni APIs**: localhost:8008, 8009, 8010
   - **etcd endpoints**: localhost:2379, 2381, 2383

3. **Connect to PostgreSQL**:
   ```bash
   # Via proxy — always goes to primary (recommended):
   psql -h localhost -p 5435 -U postgres
   # Password: supersecretpassword

   # Direct to node 1:
   psql -h localhost -p 5432 -U postgres
   ```

The local setup includes:
- **3-node PostgreSQL cluster** with automatic failover
- **Mock Flux API server** (FastAPI with dynamic admin control endpoints)
- **Isolated Docker network** simulating real deployment
- **All services** running on separate ports for testing

### Integration Test Suite

A pytest-based integration test suite is included under `tests/` for verifying cluster behaviour under failure scenarios.

**Install dependencies**:
```bash
pip install -r requirements-test.txt
```

**Run tests** (uses `docker-compose.test.yml` with fast timing overrides):
```bash
# All scenarios
pytest tests/ -v

# Individual scenarios
pytest tests/scenarios/test_normal_failover.py -v
pytest tests/scenarios/test_ec1_unreachable_node.py -v
pytest tests/scenarios/test_ec2_late_api_registration.py -v
pytest tests/scenarios/test_ec3_majority_replacement.py -v
```

The test suite automatically builds images, starts a fresh cluster per module, and tears it down after. The `docker-compose.test.yml` override shortens all timeouts for faster test runs:

| Override | Test value | Purpose |
|---|---|---|
| `UPDATE_INTERVAL_SECONDS` | `15` | Faster updater loop |
| `DESIRED_STATE_STABILITY_CYCLES` | `2` | 30s stability window |
| `PATRONI_TTL` | `15` | Faster leader failover |
| `PATRONI_LOOP_WAIT` | `5` | Faster Patroni loop |

The mock Flux API (`mock-api/server.py`) exposes admin endpoints for injecting failures mid-test:
- `POST /admin/set-nodes` — update the node list the cluster sees
- `POST /admin/set-delay` — simulate API latency
- `POST /admin/set-error` — inject API errors
- `POST /admin/reset` — restore initial state


### Logs

Check logs for each component:
```bash
/var/log/supervisor/patroni.out.log
/var/log/supervisor/etcd.out.log
/var/log/supervisor/updater.out.log
/var/log/supervisor/proxy.out.log
```
