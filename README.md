# Flux PostgreSQL Cluster
![Version](https://img.shields.io/badge/version-1.5.2-blue.svg)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14-blue.svg)
![Patroni](https://img.shields.io/badge/Patroni-latest-green.svg)
![Docker](https://img.shields.io/badge/Docker-required-blue.svg)

This project creates a self-configuring, highly-available PostgreSQL cluster that dynamically discovers its members through the Flux API. The cluster uses Patroni for PostgreSQL high availability, etcd for distributed coordination, and automatically adapts to nodes being added or removed from the environment.

## Prerequisites

- Docker
- Docker Compose
- Access to Flux network for API calls

## Docker image tags

| Tag | Branch | PostgreSQL |
|-----|--------|------------|
| `latest` | `master` | 14 |
| `dev` | `development` | 14 |
| `pg15` | `master` | 15 |
| `dev-pg15` | `development` | 15 |

Use `latest` / `dev` for existing PostgreSQL 14 clusters. Use `pg15` / `dev-pg15` only for **new** clusters with fresh volumes — do not swap tags on existing PG 14 data directories.

### pgvector

The PostgreSQL 14 and 15 images include the
[pgvector](https://github.com/pgvector/pgvector) server extension. It is
installed on every HA node but is not enabled automatically, so upgrading an
existing cluster does not modify any database. After every node has been
redeployed with a pgvector-capable image, enable it once in each database that
needs vector support:

```sql
CREATE EXTENSION IF NOT EXISTS vector;
```

Run the command through port 5433 so it reaches the current primary. Physical
WAL replication carries the extension catalog, vector tables, data, and indexes
to the replicas; the package installed in every image supplies the server-side
library after failover. Each node continues to use its own persistent
`/var/lib/postgresql/data` volume—do not share or filesystem-sync `PGDATA`
between nodes.

For an existing cluster, do not enable `vector` until all potential primary
nodes have been upgraded. A node running an older image does not have the
extension library and is not a safe failover target for databases that use it.

## Quick Start

New clusters require an explicit offline `flux-agent create-cluster` operation.
Existing deployments with persistent PGDATA and etcd can upgrade without adding
environment variables: nodes record their local database identity automatically,
and empty replacements learn it from a verified healthy initialized cluster.
`EXPECTED_POSTGRES_SYSTEM_ID` is an optional additional identity constraint.
See [creation and recovery](CLUSTER_SAFETY.md).

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
• After a failover the proxy verifies the new primary and closes connections to invalidated targets
• PostgreSQL instances replicate data across nodes via public internet
• Only PRIMARY accepts writes; SECONDARY nodes are read-only
```

1. **Deploy on Flux**:
  - Log in to home.runonflux.io and navigate to Applications > Register New App.
  - Add a component for PostgreSQL.
  - Use the official Docker image: `runonflux/flux-pg-cluster:latest` (PostgreSQL 14).
  - For PostgreSQL 15, use `runonflux/flux-pg-cluster:pg15` with **fresh volumes** on all nodes.
  - Add persistent Container Data for PostgreSQL at `/var/lib/postgresql/data`.
  - Add persistent Container Data for etcd at `/var/lib/etcd`. Without this,
    a simultaneous replacement or restart of all instances loses the complete
    Patroni DCS. The agent can recover automatically when the surviving PGDATA
    copies unanimously identify one safe authority. If the former primary was
    rescheduled away, the most advanced replica on the common timeline is
    selected deterministically. Persisting etcd avoids the recovery event and
    remains strongly recommended.
  - **If you enable the built-in backups, add a separate persistent Container Data volume at `/var/lib/postgresql/backups`.** This is a sibling of the PostgreSQL data directory, so persisting `/var/lib/postgresql/data` does not persist backups. Without this volume, backups disappear when Flux replaces or reschedules the container. The image initializes the mounted directory with ownership for the `postgres` user at startup.
  - Add these ports to the `Cont. Ports` field: `[5432,5433,8008,2379,2380]`.
  - Using the `Ports` field, map those ports to new ones, for example: `[15432,15433,18008,12379,12380]`.
  - For the `Domains` field, add this: `["","","","",""]`.
  - Use the following sample to set the environment variables for the PostgreSQL component:

   ```json
   [
      "HOST_POSTGRES_PORT=15432",
      "HOST_PROXY_PORT=15433",
      "HOST_PATRONI_API_PORT=18008",
      "HOST_ETCD_CLIENT_PORT=12379",
      "HOST_ETCD_PEER_PORT=12380",
      "POSTGRES_SUPERUSER_PASSWORD=your-super-secret-password",
      "POSTGRES_REPLICATION_PASSWORD=your-replication-password",
      "POSTGRES_DB=your-app-database",
      "SSL_ENABLED=true",
      "SSL_PASSPHRASE=your-ssl-passphrase",
      "ALLOW_NEW_CLUSTER_BOOTSTRAP=false",
      "AUTO_BOOTSTRAP_IF_FRESH=false",
      "ALLOW_PG_DATA_WIPE=false"
   ]
   ```

   Normal startup never creates a PostgreSQL cluster. Follow [explicit creation and migration](CLUSTER_SAFETY.md) for new databases. Existing persistent deployments do not need a new ENV for rollout. Empty replacements verify the running cluster before cloning; an optional `EXPECTED_POSTGRES_SYSTEM_ID` adds an independent constraint. Without a surviving authenticated cluster or known identity, a replacement waits for recovery and never initializes a new database.
    

2. **Connect from other Flux components**:
   ```bash
   # Recommended — connect via proxy (always routes to current primary):
   postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5433/[POSTGRES_DB]

   # With SSL (recommended):
   postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5433/[POSTGRES_DB]?sslmode=require

   # Direct connection to a specific node (bypasses proxy — use only for read replicas or diagnostics):
   postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5432/[POSTGRES_DB]
   ```
   > Replace `[PG_COMPONENT_NAME]` with the name you gave the PostgreSQL component in your Flux app (e.g. `pg`). Example: `postgresql://postgres:password@pg:5433/umami`

3. **Monitor your cluster**:
   - Access Patroni REST API: `https://your-app-name.app_{patroni_rest_api_port}.runonflux.io`


## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HOST_POSTGRES_PORT` | Host PostgreSQL port mapping | `5432` |
| `HOST_PROXY_PORT` | Host mapping for proxy port 5433. Also carries the authenticated pre-bootstrap identity probe. Defaults to the port offset implied by `HOST_POSTGRES_PORT` and `PROXY_LISTEN_PORT` (for example, `15433` when PostgreSQL is `15432`). Set it explicitly for non-consecutive mappings. | derived |
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
| `EXPECTED_POSTGRES_SYSTEM_ID` | Optional explicit database identity constraint. Existing volumes derive their own identity; blank replacements learn and persist it from an authenticated, quorum-backed initialized cluster before cloning. Conflicting or unavailable evidence blocks learning. | empty |
| `CLUSTER_SEED_IPS` | Optional comma-separated known peer IPs, probed in addition to discovery and persisted membership. | empty |
| `RETIRED_CLUSTER_IPS` | Operator-confirmed fenced former peers excluded from active discovery/recovery. Never use merely because Flux omitted a node. | empty |
| `ALLOW_NEW_CLUSTER_BOOTSTRAP` | Deprecated compatibility setting; cannot authorize initdb. Use `flux-agent create-cluster`. | `false` |
| `ALLOW_ANY_NODE_BOOTSTRAP` | Deprecated compatibility setting; cannot authorize initdb. | `false` |
| `AUTO_BOOTSTRAP_IF_FRESH` | Deprecated compatibility setting; empty-peer agreement never authorizes initdb. | `false` |
| `DEAD_CLUSTER_RECOVERY` | Allow rebuilding DCS around pinned, preserved PGDATA after repeated unanimous evidence from all remembered active peers. Unknown/unreachable former peers block recovery until the operator fences and retires them. Does not authorize initdb. | `true` |
| `ALLOW_PG_DATA_WIPE` | Deprecated compatibility setting; mismatched PGDATA is preserved even when this is true. Operator-verified replacement is manual. | `false` |
| `ETCD_JOIN_MAX_RETRIES` | Peer-join attempts made by the deterministic candidate before evaluating bootstrap. Non-candidates wait at least 60 attempts and never bootstrap themselves. | `12` |
| `ETCD_JOIN_RETRY_DELAY_SECONDS` | Delay between peer-join retries | `10` |
| `BOOTSTRAP_PEER_CONFIRM_CYCLES` | Consecutive unanimous confirmations required before control-plane recovery | `3` |
| `BOOTSTRAP_PEER_PROBE_INTERVAL_SECONDS` | Delay between recovery confirmation cycles | `10` |
| `BOOTSTRAP_PEER_PROBE_TIMEOUT_SECONDS` | Timeout for each peer identity probe | `5` |
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
| `BACKUP_ENABLED` | Enable periodic `pg_dumpall` logical backups (taken only on the healthy primary). | `false` |
| `BACKUP_INTERVAL_SECONDS` | How often a backup is attempted. Minimum 60. | `86400` (daily) |
| `BACKUP_DIR` | Directory backups are written to. Map this to a persistent / off-node volume for real durability. | `/var/lib/postgresql/backups` |
| `BACKUP_RETENTION_COUNT` | Number of most-recent backups to keep. Older ones are pruned after each successful backup. | `1` |
| `BACKUP_MAX_TOTAL_BYTES` | Cap on total backup storage in bytes (`0` = unlimited). After retention pruning, oldest backups are dropped until the total is under this cap (the newest is always kept). | `0` |

### Split-Brain & Data-Loss Prevention Controls

- **First creation is an explicit offline operator operation.** Normal startup never runs `initdb`, regardless of legacy bootstrap flags.
- **Remember the original database identity automatically.** Existing PGDATA supplies it; empty replacements require authenticated, quorum-backed agreement before learning it. `EXPECTED_POSTGRES_SYSTEM_ID` is an optional extra constraint, not a rollout prerequisite.
- **Preserve healthy members.** Discovery omissions never remove a reachable healthy member or self; unreachable removals need quorum and repeated failures.
- **Preserve PostgreSQL data.** A mismatched identity is quarantined. The proxy rejects unknown/foreign identities and conflicting primaries, clears unverified targets, and closes connections to invalidated targets.
- **Recovery requires evidence from remembered active peers.** After majority loss, fence and explicitly retire unreachable former placements before authorizing control-plane recovery.
- See [CLUSTER_SAFETY.md](CLUSTER_SAFETY.md) for creation, upgrades, replacement, disaster recovery, and backup requirements.

### Recovering the cluster from a backup

Restore a verified physical backup to one stopped node, retain its system ID in
the recovery records (optionally also as `EXPECTED_POSTGRES_SYSTEM_ID`), and follow the fencing and
retirement procedure in [CLUSTER_SAFETY.md](CLUSTER_SAFETY.md). Empty replicas
clone the recovered database. Unknown former writers block recovery until they
are accounted for. A logical restore requires an explicitly created new cluster
and a deliberately updated deployment identity.

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

1. **Discovery Phase**: Combine Flux locations with durable membership, configured seeds, and self. Flux locations are discovery hints, not complete membership authority.
2. **Configuration Generation**: Creates Patroni and etcd configuration files using discovered IPs
3. **Service Startup**: Supervisord starts etcd, then Patroni, then the cluster update daemon

### Dynamic Membership

- **Background Process**: Continuously monitors Flux API (every 60 seconds)
- **Conservative Removal**: Never removes self or a reachable healthy member. Removing an omitted unreachable member requires quorum and repeated direct failures.
- **Self-Registration**: New nodes automatically join the cluster when they start up

### Service Management

The supervisord configuration manages five main processes:

- **etcd**: Distributed key-value store for cluster coordination
- **patroni**: PostgreSQL high availability manager
- **updater**: Background daemon that maintains cluster membership
- **proxy**: TCP primary-routing proxy on port 5433 (disable with `PROXY_ENABLED=false`)
- **backup**: Periodic `pg_dumpall` backup agent (opt-in via `BACKUP_ENABLED=true`)

### Backups

> An HA cluster protects against a node failing — it does **not** protect against a bad epoch, an operator mistake, or accidental data loss. Always keep backups of anything you care about.

Set `BACKUP_ENABLED=true` to run the built-in logical-backup agent. Behaviour:

- Runs only on the node Patroni reports as a **healthy, running primary**, so you get a single authoritative copy rather than one per node.
- Each run streams `pg_dumpall` (compressed) to a temp file and **integrity-checks** it (valid gzip + the pg_dumpall completion marker) before it replaces anything. If the database/cluster is unhealthy or the dump is truncated, the previous good backups are left untouched — **a broken dump never overwrites a good backup**.
- After a successful backup it prunes: keeps the newest `BACKUP_RETENTION_COUNT` files (default **1**), then drops oldest files until total size is under `BACKUP_MAX_TOTAL_BYTES` (default `0` = unlimited). The just-made backup is never pruned, so the directory does not grow unbounded.

```json
"BACKUP_ENABLED=true",
"BACKUP_INTERVAL_SECONDS=86400",
"BACKUP_DIR=/var/lib/postgresql/backups",
"BACKUP_RETENTION_COUNT=1",
"BACKUP_MAX_TOTAL_BYTES=0"
```

> **Required for durable built-in backups:** when creating the Flux app, add `/var/lib/postgresql/backups` as persistent Container Data. Persisting `/var/lib/postgresql/data` does **not** cover this sibling directory. Without a separate persistent or off-node destination, backup files are lost when the container is replaced or rescheduled. For point-in-time recovery beyond a daily dump, layer on continuous WAL archiving (pgBackRest / WAL-G).

**Restore** from a dump (on a fresh primary):

```bash
gunzip -c /var/lib/postgresql/backups/pgdumpall-<TS>.sql.gz | psql -h 127.0.0.1 -p 5432 -U postgres
```

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
Host: [PG_COMPONENT_NAME]   (the component name you set in Flux, e.g. "pg")
Port: 5433
Database: [POSTGRES_DB]
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5433/[POSTGRES_DB]

# With SSL enabled:
postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5433/[POSTGRES_DB]?sslmode=require
```

**Direct connection (bypasses proxy — use only for read replicas or diagnostics):**
```
Host: [PG_COMPONENT_NAME]
Port: 5432
Database: [POSTGRES_DB]
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

postgresql://postgres:[PASSWORD]@[PG_COMPONENT_NAME]:5432/[POSTGRES_DB]
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

# Exercise the PostgreSQL 15 variant, including explicit creation and recovery
pytest tests/ -v --postgres-major=15

# Optional rolling security-build upgrade from a previous safety-enabled etcd 3.5 image
pytest tests/scenarios/test_package_upgrade.py -v --upgrade-from-image=OLD_IMAGE

# Individual scenarios
pytest tests/scenarios/test_normal_failover.py -v
pytest tests/scenarios/test_ec1_unreachable_node.py -v
pytest tests/scenarios/test_ec2_late_api_registration.py -v
pytest tests/scenarios/test_ec3_majority_replacement.py -v
```

The harness explicitly creates and pins each fresh test cluster. Readiness requires a writable primary and direct live-member probes; cached DCS membership alone is insufficient. Use `pytest tests/unit/ -q` for the harness regression checks. After building the selected PostgreSQL variant from the current runtime source, `--skip-image-build` reuses those images while iterating on tests. The two variants use the same local Compose image names, so run them sequentially and rebuild when switching majors.

The image audit also runs `tests/runtime/check_patroni_api_timeouts.py` inside each PostgreSQL image. It occupies all five API workers with silent TLS clients or completed TLS connections that never send HTTP, then verifies that normal API requests recover within bounded time. The Go client tests cover repeated cancelled probes against a silent TLS peer and release of discarded transports.

The test suite automatically builds images, starts a fresh cluster per module, and tears it down after. The `docker-compose.test.yml` override shortens all timeouts for faster test runs:

| Override | Test value | Purpose |
|---|---|---|
| `UPDATE_INTERVAL_SECONDS` | `5` | Faster updater loop |
| `DESIRED_STATE_STABILITY_CYCLES` | `2` | Repeated equal discovery views |
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

### Dependency maintenance

The agent is built with Go 1.27.4. Patroni runs under `/opt/patroni/bin/python3` in an isolated environment installed from `requirements-runtime.txt`, which pins all runtime Python packages and their PyPI SHA256 hashes. Refresh the lock for Ubuntu 24.04's Python 3.12, including transitive dependencies, and run `pip check`, vulnerability scans, and the cluster scenarios before publishing. The newer runtime supports dnspython 2.9.0, which fixes CVE-2026-114605 and requires Python 3.11 or later. Python installation tools and C development packages are excluded from the runtime image.

Version 1.5.2 backports Patroni API socket timeouts through `scripts/patch-patroni-api.py`: TLS handshakes are bounded to two seconds, HTTP request/response socket I/O to five seconds, and TLS shutdown to one second. The script checks the pinned API source checksum and stops the build if a dependency upgrade changes it; review or remove the backport when updating Patroni. The agent also bounds its outgoing TLS handshakes and closes discarded probe transports. These protections apply without new deployment ENVs and support staggered updates with older peers. If direct SQL works while the proxy reports no primary and the API stalls during TLS, inspect API worker/socket exhaustion before attempting any PostgreSQL or etcd recovery.

`bin/etcd` and `bin/etcdctl` are Linux/amd64 **etcd 3.5.35** executables rebuilt from the pinned upstream source with `golang.org/x/net` upgraded to **v0.62.0**, fixing CVE-2026-78661. See [build provenance and reproduction](bin/ETCD_BUILD.md). `bin/SHA256SUMS` verifies both executables during image builds. Rebuild both binaries and update the checksums together when upgrading; an APT refresh does not update these embedded Go dependencies.

The package-audit workflow checks Go vulnerabilities and builds/scans both PostgreSQL variants and the mock API on changes and weekly. Fixable high/critical image findings fail the workflow. Dependabot checks Docker, Python, and Actions dependencies weekly. Image publishing uses fresh base images and uncached package installation to pick up Ubuntu security backports, and records the source commit in the `org.opencontainers.image.revision` label.

PostgreSQL minor releases are installed from PGDG. Keep major-version migrations separate from package refreshes: PostgreSQL 14 support ends November 12, 2026, and changing an image tag cannot migrate an existing data directory.

## Related Projects

- Flux Postgres Cluster: [https://github.com/RunOnFlux/flux-pg-cluster](https://github.com/RunOnFlux/flux-pg-cluster)
- Flux MongoDB Cluster: [https://github.com/RunOnFlux/flux-mongodb-cluster](https://github.com/RunOnFlux/flux-mongodb-cluster)
- Flux Redis Cluster: [https://github.com/RunOnFlux/flux-redis-cluster](https://github.com/RunOnFlux/flux-redis-cluster)
