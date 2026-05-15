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
    SSL_PARAMS="--cert-file=/etc/ssl/cluster/etcd/client.crt --key-file=/etc/ssl/cluster/etcd/client.key --trusted-ca-file=/etc/ssl/cluster/ca/ca.crt --client-cert-auth --peer-cert-file=/etc/ssl/cluster/etcd/peer.crt --peer-key-file=/etc/ssl/cluster/etcd/peer.key --peer-trusted-ca-file=/etc/ssl/cluster/ca/ca.crt --peer-client-cert-auth"
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

EXPECTED_MEMBER_COUNT=$(echo "$ETCD_INITIAL_CLUSTER" | tr ',' '\n' | grep -c '=')
BOOTSTRAP_CANDIDATE_NAME=$(echo "$ETCD_INITIAL_CLUSTER" | tr ',' '\n' | sed 's/=.*//' | sort | head -n1)
ALLOW_NEW_CLUSTER_BOOTSTRAP=${ALLOW_NEW_CLUSTER_BOOTSTRAP:-false}
ALLOW_ANY_NODE_BOOTSTRAP=${ALLOW_ANY_NODE_BOOTSTRAP:-false}
AUTO_BOOTSTRAP_IF_FRESH=${AUTO_BOOTSTRAP_IF_FRESH:-true}

echo "  EXPECTED_MEMBER_COUNT=${EXPECTED_MEMBER_COUNT}"
echo "  BOOTSTRAP_CANDIDATE_NAME=${BOOTSTRAP_CANDIDATE_NAME}"
echo "  ALLOW_NEW_CLUSTER_BOOTSTRAP=${ALLOW_NEW_CLUSTER_BOOTSTRAP}"
echo "  ALLOW_ANY_NODE_BOOTSTRAP=${ALLOW_ANY_NODE_BOOTSTRAP}"
echo "  AUTO_BOOTSTRAP_IF_FRESH=${AUTO_BOOTSTRAP_IF_FRESH}"

# Extract other members' IPs from ETCD_INITIAL_CLUSTER
OTHER_IPS=$(echo "$ETCD_INITIAL_CLUSTER" | tr ',' '\n' | grep -v "$MY_NAME=" | sed 's/.*=.*:\/\///' | sed 's/:.*//')

# Function to check if a peer cluster is reachable and attempt to join it
# Pass FORCE_REJOIN=1 when local data was wiped — forces member remove+add instead of
# treating an existing registration as a normal restart (which would panic on empty raft log).
# Return codes:
#   0 = successfully joined
#   1 = no peers reachable (safe to bootstrap new if truly a fresh cluster)
#   2 = peers reachable but join failed (unsafe to bootstrap — would cause split-brain)
try_join_existing_cluster() {
    local FORCE_REJOIN="${1:-0}"
    local ANY_PEER_REACHABLE=0

    for PEER_IP in $OTHER_IPS; do
        PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"
        echo "  Trying peer at: $PEER_CLIENT_URL"

        if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=5s member list >/dev/null 2>&1; then
            echo "  Found existing cluster via $PEER_IP"
            ANY_PEER_REACHABLE=1

            # Check if this node is already a member
            EXISTING_MEMBER=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member list 2>/dev/null | grep "$MY_NAME" || true)

            if [ -n "$EXISTING_MEMBER" ]; then
                # Detect ghost (unstarted or empty clientURLs)
                GHOST_CHECK=$(echo "$EXISTING_MEMBER" | grep -E "\[unstarted\]|clientURLs= " || true)

                if [ -n "$GHOST_CHECK" ] || [ "$FORCE_REJOIN" = "1" ]; then
                    if [ -n "$GHOST_CHECK" ]; then
                        echo "  Found ghost registration (empty clientURLs) — removing and re-adding..."
                    else
                        echo "  Data was wiped — removing stale member entry to force fresh sync..."
                    fi
                    EXISTING_ID=$(echo "$EXISTING_MEMBER" | cut -d: -f1 | tr -d ' ')
                    etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" member remove "$EXISTING_ID" 2>&1 || true
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

    # Peers were reachable but we couldn't complete the join — signal split-brain risk
    if [ "$ANY_PEER_REACHABLE" -eq 1 ]; then
        return 2
    fi
    return 1
}

# Function to verify this node's etcd is in the correct cluster
verify_cluster_id() {
    local LOCAL_URL="${PROTOCOL}://127.0.0.1:${ETCD_CLIENT_PORT}"
    local PEERS_REACHABLE=0
    local PEERS_MATCHING=0
    local PEERS_MISMATCHED=0
    echo "  Verifying cluster ID..."

    # Get our local cluster ID from member list output
    local LOCAL_OUTPUT=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$LOCAL_URL" --timeout=5s member list 2>/dev/null || true)
    if [ -z "$LOCAL_OUTPUT" ]; then
        echo "  Cannot query local etcd yet"
        return 1
    fi

    # Evaluate all reachable peers and use majority to avoid false positives.
    for PEER_IP in $OTHER_IPS; do
        PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"

        # Check if peer is reachable
        local PEER_OUTPUT=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=5s member list 2>/dev/null || true)
        if [ -z "$PEER_OUTPUT" ]; then
            continue
        fi

        PEERS_REACHABLE=$((PEERS_REACHABLE + 1))

        # If peer is reachable, check if it knows about us
        if echo "$PEER_OUTPUT" | grep -q "$MY_NAME"; then
            echo "  Peer $PEER_IP knows about us"
            PEERS_MATCHING=$((PEERS_MATCHING + 1))
        else
            echo "  Peer $PEER_IP does NOT know about us — cluster ID mismatch!"
            PEERS_MISMATCHED=$((PEERS_MISMATCHED + 1))
        fi
    done

    if [ "$PEERS_REACHABLE" -eq 0 ]; then
        echo "  No peers reachable to verify cluster ID"
        return 1
    fi

    echo "  Peer verification summary: reachable=${PEERS_REACHABLE}, matching=${PEERS_MATCHING}, mismatched=${PEERS_MISMATCHED}"
    if [ "$PEERS_MISMATCHED" -gt "$PEERS_MATCHING" ]; then
        return 2
    fi

    return 0
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

    # Disable set -e for this call: verify_cluster_id returns non-zero on mismatch/unreachable,
    # and set -e would exit the script before we can capture and act on the result.
    set +e
    verify_cluster_id
    VERIFY_RESULT=$?
    set -e

    # Kill the temporary etcd
    kill $ETCD_PID 2>/dev/null || true
    wait $ETCD_PID 2>/dev/null || true
    sleep 2

    if [ $VERIFY_RESULT -eq 2 ]; then
        echo "  CLUSTER ID MISMATCH DETECTED — wiping data and rejoining..."
        rm -rf /var/lib/etcd/*
        FORCE_REJOIN=1
        # Fall through to the "no data directory" path below
    elif [ $VERIFY_RESULT -eq 0 ]; then
        CLUSTER_STATE=existing
        echo "  CLUSTER_STATE=existing (verified — same cluster)"
    else
        # Local etcd didn't respond — check if peers see us as a full member.
        # If they do, our local data is corrupt (empty raft log): wipe and force-rejoin
        # so the leader sends a snapshot instead of a heartbeat (which would cause a panic).
        PEER_SEES_US_AS_FULL=0
        for PEER_IP in $OTHER_IPS; do
            PEER_CLIENT_URL="${PROTOCOL}://${PEER_IP}:${HOST_ETCD_CLIENT_PORT}"
            PEER_OUTPUT=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_CLIENT_URL" --timeout=5s member list 2>/dev/null || true)
            if [ -n "$PEER_OUTPUT" ]; then
                OUR_ENTRY=$(echo "$PEER_OUTPUT" | grep "$MY_NAME" || true)
                if [ -n "$OUR_ENTRY" ]; then
                    GHOST_CHECK=$(echo "$OUR_ENTRY" | grep -E "\[unstarted\]|clientURLs= " || true)
                    if [ -z "$GHOST_CHECK" ]; then
                        echo "  Peer $PEER_IP sees us as a full member but local etcd did not respond — data is likely corrupt"
                        PEER_SEES_US_AS_FULL=1
                    fi
                fi
                break
            fi
        done

        if [ $PEER_SEES_US_AS_FULL -eq 1 ]; then
            echo "  CORRUPT DATA DETECTED — wiping and force rejoining to receive fresh snapshot..."
            rm -rf /var/lib/etcd/*
            FORCE_REJOIN=1
        else
            # No peers reachable or we appear as ghost — trust existing data
            CLUSTER_STATE=existing
            echo "  CLUSTER_STATE=existing (data directory found, no peers to verify)"
        fi
    fi
fi

# If we still don't have a cluster state (no data dir, or data was wiped above)
if [ -z "$CLUSTER_STATE" ]; then
    echo "  No data directory — checking if an existing cluster is running..."

    # Retry peer discovery with backoff (peers may still be starting)
    MAX_RETRIES=${ETCD_JOIN_MAX_RETRIES:-12}
    RETRY_DELAY=${ETCD_JOIN_RETRY_DELAY_SECONDS:-10}
    PEERS_WERE_REACHABLE=0
    for i in $(seq 1 $MAX_RETRIES); do
        echo "  Peer discovery attempt $i/$MAX_RETRIES..."
        set +e
        try_join_existing_cluster "${FORCE_REJOIN:-0}"
        JOIN_RESULT=$?
        set -e
        if [ $JOIN_RESULT -eq 0 ]; then
            break
        fi
        if [ $JOIN_RESULT -eq 2 ]; then
            PEERS_WERE_REACHABLE=1
            echo "  Peers reachable but join failed — existing cluster detected; will not bootstrap new"
        fi

        if [ $i -lt $MAX_RETRIES ]; then
            echo "  Retrying in ${RETRY_DELAY}s..."
            sleep $RETRY_DELAY
        fi
    done

    # If we never found an existing cluster, only bootstrap when explicitly allowed.
    # This prevents accidental split clusters during network partitions/flapping.
    if [ -z "$CLUSTER_STATE" ]; then
        if [ "${FORCE_REJOIN:-0}" = "1" ]; then
            echo "  Rejoin was forced after local data wipe, but no reachable peer was found. Refusing unsafe bootstrap."
            exit 1
        fi

        # If peers were reachable at any point but join failed, there IS an existing cluster.
        # Bootstrapping a new cluster here would create split-brain with a different cluster ID.
        # Exit and let supervisord restart so we retry joining.
        if [ "$PEERS_WERE_REACHABLE" -eq 1 ]; then
            echo "  SPLIT-BRAIN GUARD: peers were reachable but join failed — existing cluster detected."
            echo "  Refusing new cluster bootstrap. Supervisord will restart and retry joining."
            exit 1
        fi

        if [ "$EXPECTED_MEMBER_COUNT" -le 1 ]; then
            CLUSTER_STATE=new
            echo "  Single-member configuration detected — bootstrapping as new"
        else
            ETCD_DATA_EMPTY=0
            POSTGRES_DATA_EMPTY=0

            if [ ! -f /var/lib/etcd/member/snap/db ]; then
                ETCD_DATA_EMPTY=1
            fi

            if [ ! -d /var/lib/postgresql/data/global ]; then
                POSTGRES_DATA_EMPTY=1
            fi

            # Safe first-bootstrap fallback for fresh installations.
            # For a brand new multi-member cluster, all empty-data nodes must be allowed to
            # start in "new" mode so etcd can form quorum. Restricting bootstrap to one node
            # would deadlock because a single member cannot serve member list/member add without quorum.
            if [ "$ALLOW_NEW_CLUSTER_BOOTSTRAP" != "true" ]; then
                if [ "$AUTO_BOOTSTRAP_IF_FRESH" = "true" ] && [ "$ETCD_DATA_EMPTY" -eq 1 ] && [ "$POSTGRES_DATA_EMPTY" -eq 1 ]; then
                    CLUSTER_STATE=new
                    if [ "$MY_NAME" = "$BOOTSTRAP_CANDIDATE_NAME" ]; then
                        echo "  Fresh multi-member install detected on deterministic bootstrap candidate."
                    else
                        echo "  Fresh multi-member install detected on non-candidate node."
                        echo "  Allowing coordinated first-bootstrap to form quorum."
                    fi
                    echo "  Auto-bootstrapping new cluster safely (data dirs are empty)."
                else
                    echo "  Multi-member cluster and no existing peer found."
                    echo "  Refusing automatic new cluster bootstrap to prevent split-brain."
                    echo "  Auto-bootstrap conditions not met:"
                    echo "    candidate=$MY_NAME==$BOOTSTRAP_CANDIDATE_NAME"
                    echo "    etcd_data_empty=$ETCD_DATA_EMPTY"
                    echo "    postgres_data_empty=$POSTGRES_DATA_EMPTY"
                    echo "    AUTO_BOOTSTRAP_IF_FRESH=$AUTO_BOOTSTRAP_IF_FRESH"
                    echo "  Set ALLOW_NEW_CLUSTER_BOOTSTRAP=true only for controlled first bootstrap if needed."
                    exit 1
                fi
            fi

            if [ -z "$CLUSTER_STATE" ] && [ "$ALLOW_ANY_NODE_BOOTSTRAP" != "true" ] && [ "$MY_NAME" != "$BOOTSTRAP_CANDIDATE_NAME" ]; then
                echo "  Bootstrap is restricted to deterministic candidate $BOOTSTRAP_CANDIDATE_NAME."
                echo "  Current node $MY_NAME is not allowed to bootstrap a new multi-member cluster."
                exit 1
            fi

            if [ -z "$CLUSTER_STATE" ]; then
                CLUSTER_STATE=new
                echo "  Explicit bootstrap override enabled — bootstrapping new cluster"
            fi
        fi
    fi
fi

echo "  CLUSTER_STATE=$CLUSTER_STATE"
echo "  SSL_PARAMS=$SSL_PARAMS"

# Check for force-new-cluster flag written by update-cluster.sh quorum recovery
FORCE_NEW_CLUSTER_FLAG=/tmp/force-new-cluster
FORCE_NEW_CLUSTER_ARG=""
if [ -f "$FORCE_NEW_CLUSTER_FLAG" ]; then
    echo "$(date): Force-new-cluster flag detected — starting etcd with --force-new-cluster"
    rm -f "$FORCE_NEW_CLUSTER_FLAG"
    FORCE_NEW_CLUSTER_ARG="--force-new-cluster"
fi

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
    $FORCE_NEW_CLUSTER_ARG \
    $SSL_PARAMS
