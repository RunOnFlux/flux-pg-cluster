# Flux PostgreSQL Cluster
![Version](https://img.shields.io/badge/version-1.0.7-blue.svg)
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
   ┌──────────────────┐       ┌──────────────────┐       ┌──────────────────┐
   │      Node 1      │       │      Node 2      │       │       Node 3     │
   │  ┌────────────┐  │       │  ┌────────────┐  │       │  ┌────────────┐  │
   │  │  Your App  │  │       │  │  Your App  │  │       │  │  Your App  │  │
   │  │ (Component)│  │       │  │ (Component)│  │       │  │ (Component)│  │
   │  └─────┬──────┘  │       │  └─────┬──────┘  │       │  └─────┬──────┘  │
   │  ┌─────▼──────┐  │       │  ┌─────▼──────┐  │       │  ┌─────▼──────┐  │
   │  │ PostgreSql │  │       │  │ PostgreSql │  │       │  │ PostgreSql │  │
   │  │Patroni+ETCD│  │       │  │Patroni+ETCD│  │       │  │Patroni+ETCD│  │
   │  │   PRIMARY  │◄─┼───────┼─►│  SECONDARY │◄─┼───────┼─►│  SECONDARY │  │
   │  │(Read+Write)│  │       │  │ (Read-Only)│  │       │  │ (Read-Only)│  │
   │  └────────────┘  │       │  └────────────┘  │       │  └────────────┘  │
   └──────────────────┘       └──────────────────┘       └──────────────────┘
            │                          │                          │ 
            └──────────────────────────┼──────────────────────────┘
                            Replication via Public Internet
Key Points:
• Each application instance connects ONLY to its local PostgreSql instance directly
• PostgreSql instances replicate data across nodes via public internet
• Only PRIMARY accepts writes; SECONDARY nodes are read-only
```

1. **Deploy on Flux**:
   - Add a component for PostgreSQL
   - Use the official Docker image: `runonflux/flux-pg-cluster:latest`
   - Set Container Data for the component to `/var/lib/postgresql/data`
   - USe the following sample to set the environment variables for PostgreSQL component:
    ```json
    [
        "APP_NAME=your-app-name",
        "HOST_POSTGRES_PORT=15432",
        "HOST_PATRONI_API_PORT=18008",
        "HOST_ETCD_CLIENT_PORT=12379",
        "HOST_ETCD_PEER_PORT=12380",
        "POSTGRES_SUPERUSER_PASSWORD=your-super-secret-password",
        "POSTGRES_REPLICATION_PASSWORD=your-replication-password",
        "SSL_ENABLED=true",
        "SSL_PASSPHRASE=your-ssl-passphrase"
    ]
    ```
    

2. **Connect from other Flux components**:
   ```bash
   # Use this connection string in your applications:
   postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/postgres

   # With SSL (recommended):
   postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/postgres?sslmode=require
   ```

3. **Monitor your cluster**:
   - Access Patroni REST API: `https://your-app-name.app_{patroni_rest_api_port}.runonflux.io`


## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `APP_NAME` | Flux application name for API discovery | `myapp-postgres` |
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
| `POSTGRES_DB` | Default PostgreSQL database | `postgres` |
| `SSL_ENABLED` | Enable SSL/TLS encryption for all services | `false` |
| `SSL_PASSPHRASE` | Deterministic passphrase for certificate generation | Required if SSL_ENABLED=true |
| `SSL_CERT_VALIDITY_DAYS` | Certificate validity period in days | `3650` |

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

The supervisord configuration manages three main processes:

- **etcd**: Distributed key-value store for cluster coordination
- **patroni**: PostgreSQL high availability manager
- **updater**: Background script that maintains cluster membership

### Access PostgreSQL

#### Connection Strings

**For connections from within Docker containers (inside the cluster network):**
```
Host: flux{COMPONENT_NAME}_{APPNAME}
Port: 5432
Database: postgres
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

Example connection string:
postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/postgres

# With SSL enabled:
postgresql://postgres:[PASSWORD]@flux{PG_COMPONENT_NAME}_{APPNAME}:5432/postgres?sslmode=require
```

**For external connections (from host machine or remote clients):**
```
Host: localhost (or server IP)
Port: [HOST_POSTGRES_PORT] (default: 5432)
Database: postgres
Username: postgres
Password: [POSTGRES_SUPERUSER_PASSWORD]

Example connection string:
postgresql://postgres:[PASSWORD]@localhost:5432/postgres

# With SSL enabled:
postgresql://postgres:[PASSWORD]@localhost:5432/postgres?sslmode=require
```

**For local testing with multiple nodes:**
- Node 1: `postgresql://postgres:[PASSWORD]@localhost:5432/postgres`
- Node 2: `postgresql://postgres:[PASSWORD]@localhost:5433/postgres`
- Node 3: `postgresql://postgres:[PASSWORD]@localhost:5434/postgres`

**With SSL enabled, add `?sslmode=require` to any connection string above.**


### Patroni REST API

Access the Patroni REST API at `http://localhost:8008` to:
- View cluster status: `GET /cluster`
- Check member status: `GET /`
- Trigger failover: `POST /failover`

## Files Overview

- **Dockerfile**: Multi-stage build for the cluster image
- **docker-compose.yml**: Service definition with networking and volumes
- **entrypoint.sh**: Initial setup script that discovers and configures the cluster
- **patroni.yml.tpl**: Template for Patroni configuration
- **update-cluster.sh**: Background daemon for maintaining cluster membership
- **supervisord.conf**: Process management configuration

### Local Testing

For local development and testing, this repository includes a complete mock environment:

1. **Start local test cluster**:
   ```bash
   docker-compose up -d --build
   ```

2. **Access local services**:
   - **Mock Flux API**: http://localhost:8080
   - **PostgreSQL nodes**:
     - Node 1: `localhost:5432`
     - Node 2: `localhost:5433`
     - Node 3: `localhost:5434`
   - **Patroni APIs**: localhost:8008, 8009, 8010
   - **etcd endpoints**: localhost:2379, 2381, 2383

3. **Connect to PostgreSQL**:
   ```bash
   # Default credentials from .env
   psql -h localhost -p 5432 -U postgres
   # Password: supersecretpassword
   ```

The local setup includes:
- **3-node PostgreSQL cluster** with automatic failover
- **Mock Flux API server** (nginx serving JSON files)
- **Isolated Docker network** simulating real deployment
- **All services** running on separate ports for testing


### Logs

Check logs for each component:
```bash
/var/log/supervisor/patroni.out.log
/var/log/supervisor/etcd.out.log
/var/log/supervisor/updater.out.log
```
