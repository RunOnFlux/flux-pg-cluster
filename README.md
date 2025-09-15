# Dynamic PostgreSQL Cluster with Patroni and Flux Integration

![Version](https://img.shields.io/badge/version-1.0.6-blue.svg)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14-blue.svg)
![Patroni](https://img.shields.io/badge/Patroni-latest-green.svg)
![Docker](https://img.shields.io/badge/Docker-required-blue.svg)

This project creates a self-configuring, highly-available PostgreSQL cluster that dynamically discovers its members through the Flux API. The cluster uses Patroni for PostgreSQL high availability, etcd for distributed coordination, and automatically adapts to nodes being added or removed from the environment.

## Architecture

- **Single Docker Image**: Contains PostgreSQL 14, etcd, Patroni, and automation scripts
- **Dynamic Discovery**: Calls Flux API to discover cluster members
- **Auto-Configuration**: Generates configuration files based on live API data
- **Self-Healing**: Periodically updates cluster membership to match API state
- **Process Management**: Uses supervisord to manage all services

## Prerequisites

- Docker
- Docker Compose
- Access to Flux network for API calls

## Quick Start

1. **Clone and configure**:
   ```bash
   git clone <repository>
   cd flux-pg-cluster
   cp .env.example .env
   ```

2. **Edit the .env file** with your configuration:
   ```bash
   # Set your Flux app name
   APP_NAME=your-postgres-app-name

   # Configure ports (defaults shown)
   POSTGRES_PORT=5432
   PATRONI_API_PORT=8008

   # Set strong passwords
   POSTGRES_SUPERUSER_PASSWORD=your-super-secret-password
   POSTGRES_REPLICATION_PASSWORD=your-replication-password
   ```

3. **Launch the cluster**:
   ```bash
   docker-compose up -d --build
   ```

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `APP_NAME` | Flux application name for API discovery | `myapp-postgres` |
| `POSTGRES_PORT` | PostgreSQL port | `5432` |
| `PATRONI_API_PORT` | Patroni REST API port | `8008` |
| `ETCD_CLIENT_PORT` | etcd client port | `2379` |
| `ETCD_PEER_PORT` | etcd peer communication port | `2380` |
| `POSTGRES_SUPERUSER_PASSWORD` | PostgreSQL superuser password | Required |
| `POSTGRES_REPLICATION_PASSWORD` | PostgreSQL replication user password | Required |

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

## Monitoring and Management

### Check Cluster Status

```bash
# View Patroni cluster status
docker exec -it <container_name> patronictl -c /etc/patroni/patroni.yml list

# Check etcd cluster members
docker exec -it <container_name> etcdctl --endpoints=http://localhost:2379 member list

# View service logs
docker-compose logs -f postgres-cluster
```

### Access PostgreSQL

```bash
# Connect to PostgreSQL
psql -h localhost -p 5432 -U postgres

# Or from within the container
docker exec -it <container_name> psql -U postgres
```

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

## Scaling

To scale the cluster:

1. Deploy additional instances with the same `APP_NAME`
2. The new nodes will automatically discover existing cluster members
3. Existing members will detect the new nodes within 60 seconds

To remove nodes:

1. Stop the container/instance
2. Remaining cluster members will detect the removal and clean up within 60 seconds

## Troubleshooting

### Common Issues

1. **Cluster fails to start**: Check if the Flux API is accessible and returns valid data
2. **Split-brain scenarios**: Ensure network connectivity between all cluster members
3. **Permission issues**: Verify Docker has proper permissions to create volumes

### Logs

Check logs for each component:
```bash
# All services
docker-compose logs

# Specific service logs
/var/log/supervisor/patroni.out.log
/var/log/supervisor/etcd.out.log
/var/log/supervisor/updater.out.log
```

## Security Considerations

- Use strong passwords for database authentication
- Consider network isolation and firewall rules
- Regularly update the base image for security patches
