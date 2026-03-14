#!/bin/bash
set -e

echo "ETCD STARTING: $(date)"

# Source cluster environment
source /etc/cluster_env

echo "ETCD CONFIG:"
echo "  NAME=$MY_NAME"
echo "  SSL_ENABLED=$SSL_ENABLED"

# Determine protocol and SSL params
if [ "$SSL_ENABLED" = "true" ]; then
    PROTOCOL=https
    SSL_PARAMS="--cert-file=/etc/ssl/cluster/etcd/client.crt --key-file=/etc/ssl/cluster/etcd/client.key --trusted-ca-file=/etc/ssl/cluster/ca/ca.crt --client-cert-auth --peer-auto-tls"
    ETCDCTL_SSL_OPTS="--cert-file=/etc/ssl/cluster/etcd/client.crt --key-file=/etc/ssl/cluster/etcd/client.key --ca-file=/etc/ssl/cluster/ca/ca.crt"
    echo "  SSL: Enabled"
else
    PROTOCOL=http
    SSL_PARAMS=""
    ETCDCTL_SSL_OPTS=""
    echo "  SSL: Disabled"
fi

echo "  CLIENT_URLS=${PROTOCOL}://0.0.0.0:${ETCD_CLIENT_PORT}"
echo "  ADVERTISE_CLIENT_URLS=${PROTOCOL}://${MY_IP}:${HOST_ETCD_CLIENT_PORT}"
echo "  PEER_URLS=${PROTOCOL}://0.0.0.0:${ETCD_PEER_PORT}"
echo "  INITIAL_ADVERTISE_PEER_URLS=${PROTOCOL}://${MY_IP}:${HOST_ETCD_PEER_PORT}"
echo "  INITIAL_CLUSTER=${ETCD_INITIAL_CLUSTER}"

# Extract other members' IPs from ETCD_INITIAL_CLUSTER
OTHER_IPS=$(echo "$ETCD_INITIAL_CLUSTER" | tr ',' '\n' | grep -v "$MY_NAME=" | sed 's/.*=.*:\/\///' | sed 's/:.*//')

# Function to check if a peer cluster is reachable and attempt to join it
try_join_existing_cluster() {
    for PEER_IP in $OTHER_IPS; do
        PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"
        echo "  Trying peer at: $PEER_CLIENT_URL"

        if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=5s cluster-health >/dev/null 2>&1; then
            echo "  Found existing cluster via $PEER_IP"

            # Check if this node is already a member
            EXISTING_MEMBER=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member list 2>/dev/null | grep "$MY_NAME" || true)

            if [ -n "$EXISTING_MEMBER" ]; then
                # Check if it has empty clientURLs or is unstarted (ghost from failed previous join)
                GHOST_CHECK=$(echo "$EXISTING_MEMBER" | grep -E "\[unstarted\]|clientURLs= " || true)
                if [ -n "$GHOST_CHECK" ]; then
                    echo "  Found ghost registration (empty clientURLs) — removing and re-adding..."
                    GHOST_ID=$(echo "$EXISTING_MEMBER" | cut -d: -f1 | tr -d ' ')
                    etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member remove "$GHOST_ID" 2>&1 || true
                    sleep 2
                    # Fall through to add below
                else
                    echo "  This node is already registered in the cluster — starting as existing"
                    CLUSTER_STATE=existing
                    return 0
                fi
            fi

            echo "  Adding this node to the existing cluster..."
            PEER_URL="${PROTOCOL}://${MY_IP}:${HOST_ETCD_PEER_PORT}"
            if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member add "$MY_NAME" "$PEER_URL" 2>&1; then
                echo "  Successfully registered in existing cluster"
                CLUSTER_STATE=existing
                return 0
            else
                echo "  WARNING: member add failed on $PEER_IP, trying next peer..."
            fi
        else
            echo "  Peer $PEER_IP not reachable"
        fi
    done
    return 1
}

# Function to verify this node's etcd is in the correct cluster
verify_cluster_id() {
    local LOCAL_URL="${PROTOCOL}://127.0.0.1:${ETCD_CLIENT_PORT}"
    echo "  Verifying cluster ID..."

    # Get our local cluster ID from member list output
    local LOCAL_OUTPUT=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$LOCAL_URL" --timeout=5s member list 2>/dev/null || true)
    if [ -z "$LOCAL_OUTPUT" ]; then
        echo "  Cannot query local etcd yet"
        return 1
    fi

    # Try each peer and compare cluster IDs by cross-querying
    for PEER_IP in $OTHER_IPS; do
        PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"

        # Check if peer is reachable
        local PEER_OUTPUT=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=5s member list 2>/dev/null || true)
        if [ -z "$PEER_OUTPUT" ]; then
            continue
        fi

        # If peer is reachable, check if it knows about us
        if echo "$PEER_OUTPUT" | grep -q "$MY_NAME"; then
            echo "  Peer $PEER_IP knows about us — same cluster"
            return 0
        else
            echo "  Peer $PEER_IP does NOT know about us — cluster ID mismatch!"
            return 2
        fi
    done

    # No peers reachable to compare — can't verify
    echo "  No peers reachable to verify cluster ID"
    return 1
}

# Determine cluster state
if [ -f /var/lib/etcd/member/snap/db ]; then
    # Existing data directory — but verify we're in the right cluster
    echo "  Data directory found — checking if we need to rejoin..."

    # Start etcd briefly in background to check cluster ID
    etcd \
        --name="$MY_NAME" \
        --listen-client-urls="${PROTOCOL}://0.0.0.0:${ETCD_CLIENT_PORT}" \
        --advertise-client-urls="${PROTOCOL}://${MY_IP}:${HOST_ETCD_CLIENT_PORT}" \
        --listen-peer-urls="${PROTOCOL}://0.0.0.0:${ETCD_PEER_PORT}" \
        --initial-advertise-peer-urls="${PROTOCOL}://${MY_IP}:${HOST_ETCD_PEER_PORT}" \
        --initial-cluster="$ETCD_INITIAL_CLUSTER" \
        --initial-cluster-state=existing \
        --initial-cluster-token=postgres-cluster-token \
        --data-dir=/var/lib/etcd \
        $SSL_PARAMS &
    ETCD_PID=$!

    # Wait for it to start
    sleep 10

    verify_cluster_id
    VERIFY_RESULT=$?

    # Kill the temporary etcd
    kill $ETCD_PID 2>/dev/null || true
    wait $ETCD_PID 2>/dev/null || true
    sleep 2

    if [ $VERIFY_RESULT -eq 2 ]; then
        echo "  CLUSTER ID MISMATCH DETECTED — wiping data and rejoining..."
        rm -rf /var/lib/etcd/*
        # Fall through to the "no data directory" path below
    elif [ $VERIFY_RESULT -eq 0 ]; then
        CLUSTER_STATE=existing
        echo "  CLUSTER_STATE=existing (verified — same cluster)"
    else
        # Can't verify (no peers reachable) — trust existing data
        CLUSTER_STATE=existing
        echo "  CLUSTER_STATE=existing (data directory found, no peers to verify)"
    fi
fi

# If we still don't have a cluster state (no data dir, or data was wiped above)
if [ -z "$CLUSTER_STATE" ]; then
    echo "  No data directory — checking if an existing cluster is running..."

    # Retry peer discovery with backoff (peers may still be starting)
    MAX_RETRIES=6
    RETRY_DELAY=10
    for i in $(seq 1 $MAX_RETRIES); do
        echo "  Peer discovery attempt $i/$MAX_RETRIES..."
        if try_join_existing_cluster; then
            break
        fi

        if [ $i -lt $MAX_RETRIES ]; then
            echo "  No peers reachable, waiting ${RETRY_DELAY}s before retry..."
            sleep $RETRY_DELAY
        fi
    done

    # If we never found an existing cluster, bootstrap as new
    if [ -z "$CLUSTER_STATE" ]; then
        CLUSTER_STATE=new
        echo "  No existing cluster found after $MAX_RETRIES attempts — bootstrapping as new"
    fi
fi

echo "  CLUSTER_STATE=$CLUSTER_STATE"
echo "  SSL_PARAMS=$SSL_PARAMS"
echo "Starting etcd..."

exec etcd \
    --name="$MY_NAME" \
    --listen-client-urls="${PROTOCOL}://0.0.0.0:${ETCD_CLIENT_PORT}" \
    --advertise-client-urls="${PROTOCOL}://${MY_IP}:${HOST_ETCD_CLIENT_PORT}" \
    --listen-peer-urls="${PROTOCOL}://0.0.0.0:${ETCD_PEER_PORT}" \
    --initial-advertise-peer-urls="${PROTOCOL}://${MY_IP}:${HOST_ETCD_PEER_PORT}" \
    --initial-cluster="$ETCD_INITIAL_CLUSTER" \
    --initial-cluster-state="$CLUSTER_STATE" \
    --initial-cluster-token=postgres-cluster-token \
    --data-dir=/var/lib/etcd \
    $SSL_PARAMS
