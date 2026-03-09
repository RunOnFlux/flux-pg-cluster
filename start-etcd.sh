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

# Determine cluster state
if [ -f /var/lib/etcd/member/snap/db ]; then
    # Existing data directory — this is a restart
    CLUSTER_STATE=existing
    echo "  CLUSTER_STATE=existing (data directory found — restart)"
else
    # No data directory — check if we're joining an existing cluster or bootstrapping
    CLUSTER_STATE=new
    echo "  No data directory found — checking if an existing cluster is running..."

    # Extract other members' IPs from ETCD_INITIAL_CLUSTER
    # Format: node-1-2-3-4=https://1.2.3.4:2380,node-5-6-7-8=https://5.6.7.8:2380
    OTHER_IPS=$(echo "$ETCD_INITIAL_CLUSTER" | tr ',' '\n' | grep -v "$MY_NAME=" | sed 's/.*=.*:\/\///' | sed 's/:.*//')

    for PEER_IP in $OTHER_IPS; do
        PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"
        echo "  Trying peer at: $PEER_CLIENT_URL"

        if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=3s cluster-health >/dev/null 2>&1; then
            echo "  Found existing cluster via $PEER_IP"

            # Check if this node is already a member
            EXISTING_MEMBER=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member list 2>/dev/null | grep "$MY_NAME" || true)

            if [ -n "$EXISTING_MEMBER" ]; then
                echo "  This node is already registered in the cluster — starting as existing"
            else
                echo "  Adding this node to the existing cluster..."
                PEER_URL="${PROTOCOL}://${MY_IP}:${HOST_ETCD_PEER_PORT}"
                etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member add "$MY_NAME" "$PEER_URL" 2>&1 || {
                    echo "  WARNING: member add failed, will try starting as new"
                    continue
                }
                echo "  Successfully registered in existing cluster"
            fi

            CLUSTER_STATE=existing
            break
        else
            echo "  Peer $PEER_IP not reachable"
        fi
    done

    echo "  CLUSTER_STATE=$CLUSTER_STATE"
fi

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
