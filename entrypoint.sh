#!/bin/bash
set -e

echo "================================================================================"
echo "FLUX POSTGRESQL CLUSTER - DYNAMIC PATRONI CLUSTER"
echo "Version: $(cat /app/VERSION 2>/dev/null || echo 'Unknown')"
echo "Starting PostgreSQL cluster initialization..."
echo "Time: $(date)"
echo "================================================================================"

# Enable debug mode (comment out to reduce log verbosity)
# set -x

# Source environment variables
source /proc/1/environ 2>/dev/null || true

echo "================================================================================"
echo "ENVIRONMENT VARIABLES DUMP"
echo "================================================================================"
echo "All environment variables:"
printenv | sort

echo "================================================================================"
echo "DOCKER ENVIRONMENT VARIABLES"
echo "================================================================================"

# Set defaults if not provided
APP_NAME=${APP_NAME:-postgres-cluster}
POSTGRES_SUPERUSER_PASSWORD=${POSTGRES_SUPERUSER_PASSWORD:-postgres}
POSTGRES_REPLICATION_PASSWORD=${POSTGRES_REPLICATION_PASSWORD:-replication}

# Port configuration
HOST_POSTGRES_PORT=${HOST_POSTGRES_PORT:-5432}
HOST_PATRONI_API_PORT=${HOST_PATRONI_API_PORT:-8008}
HOST_ETCD_CLIENT_PORT=${HOST_ETCD_CLIENT_PORT:-2379}
HOST_ETCD_PEER_PORT=${HOST_ETCD_PEER_PORT:-2380}

POSTGRES_PORT=${POSTGRES_PORT:-5432}
PATRONI_API_PORT=${PATRONI_API_PORT:-8008}
ETCD_CLIENT_PORT=${ETCD_CLIENT_PORT:-2379}
ETCD_PEER_PORT=${ETCD_PEER_PORT:-2380}

echo "Configuration variables after assignment:"
echo "  APP_NAME: $APP_NAME"
echo "  HOST_POSTGRES_PORT: $HOST_POSTGRES_PORT"
echo "  HOST_PATRONI_API_PORT: $HOST_PATRONI_API_PORT"
echo "  HOST_ETCD_CLIENT_PORT: $HOST_ETCD_CLIENT_PORT"
echo "  HOST_ETCD_PEER_PORT: $HOST_ETCD_PEER_PORT"
echo "  POSTGRES_PORT: $POSTGRES_PORT"
echo "  PATRONI_API_PORT: $PATRONI_API_PORT"
echo "  ETCD_CLIENT_PORT: $ETCD_CLIENT_PORT"
echo "  ETCD_PEER_PORT: $ETCD_PEER_PORT"
echo "  POSTGRES_SUPERUSER_PASSWORD: [REDACTED]"
echo "  POSTGRES_REPLICATION_PASSWORD: [REDACTED]"

echo "================================================================================"
echo "IP DISCOVERY"
echo "================================================================================"

# Get the host IP address
echo "Attempting to discover host IP address..."

# For local testing with Docker, prefer container IP over external IP
if [ -n "$FLUX_API_URL" ] && echo "$FLUX_API_URL" | grep -q "172.20.0.5"; then
    echo "Local testing mode detected - using container IP"
    echo "Method 1: Container hostname to IP mapping"

    # Map hostname to known container IPs for local testing
    case "$(hostname)" in
        "postgres-cluster-node1") MY_IP="172.20.0.10" ;;
        "postgres-cluster-node2") MY_IP="172.20.0.11" ;;
        "postgres-cluster-node3") MY_IP="172.20.0.12" ;;
        *)
            echo "Unknown hostname $(hostname), trying network interface detection"
            MY_IP=$(hostname -i 2>&1 | awk '{print $1}') || echo "Failed"
            ;;
    esac
    echo "Result: $MY_IP"
else
    echo "Production mode - using external IP"
    echo "Method 1: curl ifconfig.me"
    MY_IP=$(curl -s http://ifconfig.me 2>&1) || echo "Failed"
    echo "Result: $MY_IP"
fi

if [ -z "$MY_IP" ] || [ "$MY_IP" = "Failed" ]; then
    echo "Method 2: curl ipinfo.io/ip"
    MY_IP=$(curl -s http://ipinfo.io/ip 2>&1) || echo "Failed"
    echo "Result: $MY_IP"
fi

if [ -z "$MY_IP" ] || [ "$MY_IP" = "Failed" ]; then
    echo "Method 3: hostname -I"
    MY_IP=$(hostname -I | awk '{print $1}' 2>&1) || echo "Failed"
    echo "Result: $MY_IP"
fi

echo "Final detected host IP: $MY_IP"

echo "================================================================================"
echo "FLUX API DISCOVERY"
echo "================================================================================"

# Call Flux API to get cluster member IPs
# Use local mock API if FLUX_API_URL is set, otherwise use real Flux API
FLUX_API_BASE="${FLUX_API_URL:-https://api.runonflux.io}"
echo "Fetching cluster members from Flux API for app: $APP_NAME"
echo "API URL: ${FLUX_API_BASE}/apps/location/${APP_NAME}"

API_RESPONSE=$(curl -s "${FLUX_API_BASE}/apps/location/${APP_NAME}" 2>&1 || echo '{"data":[]}')
echo "API Response:"
echo "$API_RESPONSE" | jq '.' 2>/dev/null || echo "$API_RESPONSE"

# Parse JSON response to extract IP addresses (strip any ports)
echo "Parsing API response..."
RAW_IPS=$(echo "$API_RESPONSE" | jq -r '.data[]?.ip // empty' 2>/dev/null | grep -v "^$")
echo "Raw IPs from API (may include ports):"
echo "$RAW_IPS"

CLUSTER_IPS=$(echo "$RAW_IPS" | sed 's/:[0-9]*$//' | sort | uniq)
echo "Cleaned cluster IPs (ports stripped):"
echo "$CLUSTER_IPS"

if [ -z "$CLUSTER_IPS" ]; then
    echo "WARNING: No cluster IPs found from API, using local IP only"
    CLUSTER_IPS="$MY_IP"
fi

echo "Final cluster IPs to use:"
echo "$CLUSTER_IPS"

echo "================================================================================"
echo "CLUSTER CONFIGURATION GENERATION"
echo "================================================================================"

# Convert IPs to arrays for processing
IPS_ARRAY=($CLUSTER_IPS)
ETCD_HOSTS=""
ETCD_INITIAL_CLUSTER=""

echo "Processing ${#IPS_ARRAY[@]} cluster members..."

# Build etcd configuration strings
for i in "${!IPS_ARRAY[@]}"; do
    IP="${IPS_ARRAY[$i]}"
    NODE_NAME="node-$(echo $IP | tr '.' '-')"

    echo "Processing member $((i+1)): IP=$IP, NODE_NAME=$NODE_NAME"

    if [ $i -gt 0 ]; then
        ETCD_HOSTS="${ETCD_HOSTS},"
        ETCD_INITIAL_CLUSTER="${ETCD_INITIAL_CLUSTER},"
    fi

    # Use mapped ports for cluster communication
    MEMBER_CLIENT_URL="${IP}:${HOST_ETCD_CLIENT_PORT}"
    MEMBER_PEER_URL="http://${IP}:${HOST_ETCD_PEER_PORT}"

    echo "  Client URL: $MEMBER_CLIENT_URL"
    echo "  Peer URL: $MEMBER_PEER_URL"

    ETCD_HOSTS="${ETCD_HOSTS}${MEMBER_CLIENT_URL}"
    ETCD_INITIAL_CLUSTER="${ETCD_INITIAL_CLUSTER}${NODE_NAME}=${MEMBER_PEER_URL}"
done

# Generate node name for this instance
MY_NAME="node-$(echo $MY_IP | tr '.' '-')"

# Check if this node is in the initial cluster, if not add it
if ! echo "$ETCD_INITIAL_CLUSTER" | grep -q "$MY_NAME="; then
    echo "WARNING: Current node $MY_NAME not found in initial cluster, adding it..."
    if [ -n "$ETCD_INITIAL_CLUSTER" ]; then
        ETCD_INITIAL_CLUSTER="${ETCD_INITIAL_CLUSTER},"
        ETCD_HOSTS="${ETCD_HOSTS},"
    fi
    ETCD_INITIAL_CLUSTER="${ETCD_INITIAL_CLUSTER}${MY_NAME}=http://${MY_IP}:${HOST_ETCD_PEER_PORT}"
    ETCD_HOSTS="${ETCD_HOSTS}${MY_IP}:${HOST_ETCD_CLIENT_PORT}"
fi

echo "================================================================================"
echo "FINAL CLUSTER CONFIGURATION"
echo "================================================================================"
echo "My node name: $MY_NAME"
echo "My IP: $MY_IP"
echo "ETCD hosts: $ETCD_HOSTS"
echo "Initial cluster: $ETCD_INITIAL_CLUSTER"

echo "================================================================================"
echo "PATRONI CONFIGURATION GENERATION"
echo "================================================================================"

echo "Generating Patroni configuration from template..."
echo "Template substitutions:"
echo "  __MY_NAME__ -> $MY_NAME"
echo "  __MY_IP__ -> $MY_IP"
echo "  __ETCD_HOSTS__ -> $ETCD_HOSTS"
echo "  __HOST_POSTGRES_PORT__ -> $HOST_POSTGRES_PORT"
echo "  __HOST_PATRONI_API_PORT__ -> $HOST_PATRONI_API_PORT"
echo "  __POSTGRES_PORT__ -> $POSTGRES_PORT"
echo "  __PATRONI_API_PORT__ -> $PATRONI_API_PORT"
echo "  __POSTGRES_SUPERUSER_PASSWORD__ -> [REDACTED]"
echo "  __POSTGRES_REPLICATION_PASSWORD__ -> [REDACTED]"

# Generate Patroni configuration from template
sed -e "s/__MY_NAME__/$MY_NAME/g" \
    -e "s/__MY_IP__/$MY_IP/g" \
    -e "s/__ETCD_HOSTS__/$ETCD_HOSTS/g" \
    -e "s/__HOST_POSTGRES_PORT__/$HOST_POSTGRES_PORT/g" \
    -e "s/__HOST_PATRONI_API_PORT__/$HOST_PATRONI_API_PORT/g" \
    -e "s/__POSTGRES_PORT__/$POSTGRES_PORT/g" \
    -e "s/__PATRONI_API_PORT__/$PATRONI_API_PORT/g" \
    -e "s/__POSTGRES_SUPERUSER_PASSWORD__/$POSTGRES_SUPERUSER_PASSWORD/g" \
    -e "s/__POSTGRES_REPLICATION_PASSWORD__/$POSTGRES_REPLICATION_PASSWORD/g" \
    /app/patroni.yml.tpl > /etc/patroni/patroni.yml

echo "Generated Patroni configuration:"
echo "--- START patroni.yml ---"
cat /etc/patroni/patroni.yml
echo "--- END patroni.yml ---"

# Create cluster environment file for supervisord
cat > /etc/cluster_env << EOF
MY_NAME=$MY_NAME
MY_IP=$MY_IP
ETCD_HOSTS=$ETCD_HOSTS
ETCD_INITIAL_CLUSTER=$ETCD_INITIAL_CLUSTER
HOST_POSTGRES_PORT=$HOST_POSTGRES_PORT
HOST_PATRONI_API_PORT=$HOST_PATRONI_API_PORT
HOST_ETCD_CLIENT_PORT=$HOST_ETCD_CLIENT_PORT
HOST_ETCD_PEER_PORT=$HOST_ETCD_PEER_PORT
POSTGRES_PORT=$POSTGRES_PORT
PATRONI_API_PORT=$PATRONI_API_PORT
ETCD_CLIENT_PORT=$ETCD_CLIENT_PORT
ETCD_PEER_PORT=$ETCD_PEER_PORT
POSTGRES_SUPERUSER_PASSWORD=$POSTGRES_SUPERUSER_PASSWORD
POSTGRES_REPLICATION_PASSWORD=$POSTGRES_REPLICATION_PASSWORD
APP_NAME=$APP_NAME
EOF

# Prepare PostgreSQL data directory for Patroni
echo "Preparing PostgreSQL data directory for Patroni..."
echo "Contents of /var/lib/postgresql/data:"
ls -la /var/lib/postgresql/data/ || echo "Directory does not exist"

# Create directory and set permissions - let Patroni handle initialization
mkdir -p /var/lib/postgresql/data
chown -R postgres:postgres /var/lib/postgresql/data
chmod 700 /var/lib/postgresql/data

echo "PostgreSQL data directory prepared. Patroni will handle initialization."

# Set proper permissions for PostgreSQL
chown -R postgres:postgres /var/lib/postgresql/data
chmod 700 /var/lib/postgresql/data

# Create etcd data directory and set permissions
echo "Setting up etcd data directory..."
mkdir -p /var/lib/etcd
chown -R root:root /var/lib/etcd
chmod 700 /var/lib/etcd  # etcd requires strict permissions

echo "================================================================================"
echo "CLUSTER ENVIRONMENT FILE"
echo "================================================================================"
echo "Generated cluster environment file for supervisord:"
echo "--- START /etc/cluster_env ---"
cat /etc/cluster_env
echo "--- END /etc/cluster_env ---"

echo "================================================================================"
echo "INITIALIZATION COMPLETE"
echo "================================================================================"
echo "Time: $(date)"
echo "All configuration files generated successfully."
echo ""
echo "IMPORTANT NOTES:"
echo "- Patroni will handle PostgreSQL initialization and startup"
echo "- PostgreSQL should NOT be started manually"
echo "- Wait for Patroni to bootstrap the cluster (may take 1-2 minutes)"
echo "- Check 'patronictl -c /etc/patroni/patroni.yml list' for cluster status"
echo "- Check 'curl localhost:8008/cluster' for REST API status"
echo ""
echo "Starting services via supervisord..."
echo "================================================================================"