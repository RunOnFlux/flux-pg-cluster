# Flux PostgreSQL Cluster
![Version](https://img.shields.io/badge/version-1.4.0-blue.svg)
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
  - Use the official Docker image: `runonflux/flux-pg-cluster:latest` (PostgreSQL 14).
  - For PostgreSQL 15, use `runonflux/flux-pg-cluster:pg15` with **fresh volumes** on all nodes.
  - Add persistent Container Data for PostgreSQL at `/var/lib/postgresql/data`.
  - Add persistent Container Data for etcd at `/var/lib/etcd`. Without this,
    a simultaneous replacement or restart of all instances loses the complete
    Patroni DCS. The agent can recover automatically when the surviving PGDATA
    copies unanimously identify one durable former primary, but persisting etcd
    avoids the recovery event and remains strongly recommended.
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
      "AUTO_BOOTSTRAP_IF_FRESH=true",
      "ALLOW_PG_DATA_WIPE=false"
   ]
   ```

   These are the recommended bootstrap safety defaults for an automatic multi-node deployment. `AUTO_BOOTSTRAP_IF_FRESH=true` forms the initial cluster only after every expected node repeatedly confirms that it is empty; the other two settings prevent an unreachable peer or system-ID mismatch from authorizing a new epoch or deleting PostgreSQL data.
    

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
| `ALLOW_NEW_CLUSTER_BOOTSTRAP` | Explicitly allow creating a brand-new etcd cluster when no peers are reachable, including an otherwise ambiguous single-node deployment. Keep `false` during normal operations; set `true` only for a deliberate first-time cluster creation, then unset it. | `false` |
| `ALLOW_ANY_NODE_BOOTSTRAP` | If `true`, bypass deterministic bootstrap-candidate restriction. Keep `false` for safety. | `false` |
| `AUTO_BOOTSTRAP_IF_FRESH` | If `true`, the deterministic candidate may create the first cluster only after **every expected peer is reachable** and repeatedly reports empty PostgreSQL and etcd state with the identical app, Patroni scope, and membership view. Unreachable is never treated as empty. Single-node automatic bootstrap is deliberately blocked as ambiguous; use `ALLOW_NEW_CLUSTER_BOOTSTRAP=true` for an intentional single-node first boot. Requires `PROXY_ENABLED=true` on every peer. | `true` |
| `DEAD_CLUSTER_RECOVERY` | If `true`, the agent may rebuild the **etcd/Patroni control plane** after every expected node repeatedly agrees on a safe PostgreSQL authority. A sole restored PGDATA copy is authoritative. After total DCS loss with multiple copies, every node must report empty etcd, all PostgreSQL system IDs must match, and exactly one copy must retain the durable primary role while the others identify as replicas. Unreachable peers, conflicting views, mismatched IDs, or multiple primaries block recovery. | `true` |
| `ALLOW_PG_DATA_WIPE` | If `true`, the updater may delete `/var/lib/postgresql/data` when this node's PostgreSQL system ID differs from the cluster's authoritative system ID (so Patroni can re-clone via `pg_basebackup`). **Defaults to `false`**: instead of deleting data, the updater logs a loud, actionable error and leaves the data intact for a human to verify. When enabled, the wipe still requires the **live primary** to confirm the authoritative system ID. Only enable temporarily, on a node you have confirmed holds stale data. | `false` |
| `ETCD_JOIN_MAX_RETRIES` | Peer-join attempts made by the deterministic candidate before evaluating bootstrap. Non-candidates wait at least 60 attempts and never bootstrap themselves. | `12` |
| `ETCD_JOIN_RETRY_DELAY_SECONDS` | Delay between peer-join retries | `10` |
| `BOOTSTRAP_PEER_CONFIRM_CYCLES` | Consecutive unanimous peer-empty confirmations required before automatic first bootstrap | `3` |
| `BOOTSTRAP_PEER_PROBE_INTERVAL_SECONDS` | Delay between fresh-state confirmation cycles | `10` |
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

- **Recommended automatic multi-node settings:** `ALLOW_NEW_CLUSTER_BOOTSTRAP=false`, `AUTO_BOOTSTRAP_IF_FRESH=true`, and `ALLOW_PG_DATA_WIPE=false`.
- **The updater never deletes PostgreSQL data by default.** `ALLOW_PG_DATA_WIPE` defaults to `false`. When a system-ID mismatch is detected, the updater requires the **live primary** (not just any re-cloned replica) to confirm the authoritative system ID, and even then only logs a loud error rather than wiping — a human must confirm and act. This is the load-bearing guard: even if a stray/empty epoch briefly appears, it can no longer cause surviving nodes to auto-delete real data.
- `AUTO_BOOTSTRAP_IF_FRESH=true` does not infer freshness from timeouts. Every expected node must answer the authenticated identity probe on its mapped proxy port and repeatedly confirm that both data stores are empty. A preserved node, unreachable node, different app/scope, or changing membership view blocks `initdb`.
- Single-node automatic first bootstrap is intentionally unavailable because an isolated replacement is indistinguishable from a genuinely new deployment. Use the explicit `ALLOW_NEW_CLUSTER_BOOTSTRAP=true` override for that case, then remove it.
- Node ordering is used only for a genuinely fresh installation. Recovery authority is selected from unanimously observed PostgreSQL data ownership, never from the lowest IP/name.
- `DEAD_CLUSTER_RECOVERY` preserves the sole authoritative PostgreSQL data directory. It never selects between multiple data copies and never wipes the authoritative node.
- Membership removals and `ETCD_INITIAL_CLUSTER` rewrites are gated by desired-state stability to reduce churn-induced drift.

> ⚠️ **Backups are not optional.** This is an HA cluster, not a backup. Replication protects against a node failing, **not** against a bad epoch, an operator mistake, or the failure modes above. Enable the built-in [backup agent](#backups) and/or configure continuous WAL archiving (pgBackRest/WAL-G) before storing anything you care about. Verify restores regularly.

### Recovering the cluster from a backup

The automatic recovery workflow is:

1. Stop or redeploy all cluster nodes.
2. Restore the selected backup into `/var/lib/postgresql/data` on **one node only**.
3. Start/redeploy the other expected nodes with genuinely empty PostgreSQL data directories. Keep the same app name, Patroni scope, membership, ports, TLS passphrase, and PostgreSQL passwords on every node.
4. Start all nodes. Their authenticated identity endpoints must be mutually reachable through the mapped proxy port.

The nodes repeatedly exchange their app/scope, membership view, PGDATA and etcd state, PostgreSQL system identifier, and durable PostgreSQL role. Recovery proceeds when either exactly one node has readable PostgreSQL data, or total DCS loss is unanimous and matching PostgreSQL copies identify exactly one former primary. The authoritative node then:

- rebuilds the single-member etcd control plane and records its restored PostgreSQL system identifier;
- clears only stale Patroni election/health keys while preserving dynamic configuration;
- promotes the restored PostgreSQL instance;
- restores required `pg_hba`, `pg_rewind`, `postgres`, and `replicator` invariants from the configured environment.

The empty nodes join etcd as learners, are promoted after synchronizing, and let Patroni obtain a fresh `pg_basebackup` from the recovered primary. Patroni uses the local `127.0.0.1:2379` etcd endpoint for its own node, avoiding public-port hairpin NAT failures.

Automatic recovery intentionally stops if any node is unreachable, more than one node contains PostgreSQL data (even when the system IDs match), or membership views disagree. In that case, do not enable a bootstrap override: first decide which copy is authoritative and make the other nodes empty. This conservative refusal is what prevents split brain and accidental selection of an older data copy.

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
