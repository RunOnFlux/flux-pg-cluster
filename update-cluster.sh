#!/bin/bash
set -e

echo "================================================================================"
echo "CLUSTER UPDATE DAEMON STARTING"
echo "================================================================================"
echo "Time: $(date)"

# Enable debug mode (comment out to reduce log verbosity)
# set -x

# Source cluster environment variables
if [ -f /etc/cluster_env ]; then
    echo "Loading cluster environment from /etc/cluster_env..."
    source /etc/cluster_env
    echo "Cluster environment loaded successfully"
else
    echo "ERROR: /etc/cluster_env not found!"
    exit 1
fi

echo "================================================================================"
echo "ENVIRONMENT VARIABLES (UPDATE DAEMON)"
echo "================================================================================"
echo "Configuration from cluster environment:"
echo "  MY_NAME: $MY_NAME"
echo "  MY_IP: $MY_IP"
echo "  APP_NAME: $APP_NAME"
echo "  HOST_ETCD_CLIENT_PORT: $HOST_ETCD_CLIENT_PORT"
echo "  HOST_ETCD_PEER_PORT: $HOST_ETCD_PEER_PORT"
echo "  ETCD_HOSTS: $ETCD_HOSTS"
echo "  SSL_ENABLED: $SSL_ENABLED"

# Configure etcdctl SSL parameters and protocol if SSL is enabled
if [ "$SSL_ENABLED" = "true" ]; then
    ETCDCTL_SSL_OPTS="--cert-file=/etc/ssl/cluster/etcd/client.crt --key-file=/etc/ssl/cluster/etcd/client.key --ca-file=/etc/ssl/cluster/ca/ca.crt"
    ETCD_PROTOCOL="https"
    echo "  ETCD SSL: Enabled (using client certificates)"
else
    ETCDCTL_SSL_OPTS=""
    ETCD_PROTOCOL="http"
    echo "  ETCD SSL: Disabled"
fi

echo "================================================================================"
echo "STARTING MONITORING LOOP"
echo "================================================================================"

while true; do
    echo "================================================================================"
    echo "CLUSTER UPDATE CYCLE - $(date)"
    echo "================================================================================"

    echo "Fetching desired cluster state from Flux API..."
    # Use local mock API if FLUX_API_URL is set, otherwise use real Flux API
    FLUX_API_BASE="${FLUX_API_URL:-https://api.runonflux.io}"
    echo "API URL: ${FLUX_API_BASE}/apps/location/${APP_NAME}"

    # Get current desired state from Flux API
    API_RESPONSE=$(curl -s "${FLUX_API_BASE}/apps/location/${APP_NAME}" 2>&1 || echo '{"data":[]}')

    echo "API Response:"
    echo "$API_RESPONSE" | jq '.' 2>/dev/null || echo "$API_RESPONSE"

    RAW_IPS=$(echo "$API_RESPONSE" | jq -r '.data[]?.ip // empty' 2>/dev/null | grep -v "^$")
    echo "Raw IPs from API (may include ports):"
    echo "$RAW_IPS"

    DESIRED_IPS=$(echo "$RAW_IPS" | sed 's/:[0-9]*$//' | sort | uniq)

    if [ -z "$DESIRED_IPS" ]; then
        echo "$(date): WARNING: No IPs found in API response, skipping update cycle"
        echo "Sleeping for 5 minutes (300 seconds)..."
        sleep 300
        continue
    fi

    echo "$(date): Desired cluster IPs (ports stripped):"
    echo "$DESIRED_IPS"
    echo "Number of desired members: $(echo "$DESIRED_IPS" | wc -l)"

    echo "Getting current etcd cluster state..."
    CURRENT_MEMBERS=""

    # For local connections within the same container, use localhost and internal port
    LOCAL_ETCD_ENDPOINT="${ETCD_PROTOCOL}://127.0.0.1:${ETCD_CLIENT_PORT}"
    # For external connections to other nodes, use external IP and host port
    EXTERNAL_ETCD_ENDPOINT="${ETCD_PROTOCOL}://${MY_IP}:${HOST_ETCD_CLIENT_PORT}"

    echo "Local etcd endpoint: $LOCAL_ETCD_ENDPOINT"
    echo "External etcd endpoint: $EXTERNAL_ETCD_ENDPOINT"
    echo "Testing etcd connection to local endpoint first..."

    # Test etcd connectivity first (try local endpoint)
    ETCD_ENDPOINT=""
    if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$LOCAL_ETCD_ENDPOINT" member list >/dev/null 2>&1; then
        echo "etcd connection successful via local endpoint"
        ETCD_ENDPOINT="$LOCAL_ETCD_ENDPOINT"
    elif etcdctl $ETCDCTL_SSL_OPTS --endpoints="$EXTERNAL_ETCD_ENDPOINT" member list >/dev/null 2>&1; then
        echo "etcd connection successful via external endpoint"
        ETCD_ENDPOINT="$EXTERNAL_ETCD_ENDPOINT"
    fi

    if [ -n "$ETCD_ENDPOINT" ]; then
        echo "Using etcd endpoint: $ETCD_ENDPOINT"

        # Get raw member list
        RAW_MEMBER_LIST=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" member list 2>/dev/null || true)
        echo "Raw etcd member list:"
        echo "$RAW_MEMBER_LIST"

        # ================================================================
        # GHOST MEMBER CLEANUP
        # Remove members that never fully started — they appear as either:
        #   "id[unstarted]: peerURLs=..." (no clientURLs at all)
        #   "id: name=... clientURLs= isLeader=..." (empty clientURLs)
        # These are nodes added via "member add" but never published.
        # ================================================================
        GHOST_MEMBERS=$(echo "$RAW_MEMBER_LIST" | grep -E "\[unstarted\]|clientURLs= " || true)
        if [ -n "$GHOST_MEMBERS" ]; then
            echo "$(date): Found ghost members:"
            echo "$GHOST_MEMBERS"
            while IFS= read -r GHOST_LINE; do
                # Extract ID — handle both "id[unstarted]:" and "id:" formats
                GHOST_ID=$(echo "$GHOST_LINE" | sed 's/\[unstarted\]//' | cut -d: -f1 | tr -d ' ')
                # Extract IP from peerURLs
                GHOST_IP=$(echo "$GHOST_LINE" | sed 's/.*peerURLs=//' | sed 's/ .*//' | sed 's|.*://||' | sed 's/:.*//')

                # Only remove if not in desired state
                if ! echo "$DESIRED_IPS" | grep -q "^${GHOST_IP}$"; then
                    echo "$(date): Removing ghost member $GHOST_IP (ID: $GHOST_ID) — not in desired state"
                    etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" member remove "$GHOST_ID" 2>&1 || \
                        echo "$(date): Failed to remove ghost member $GHOST_ID"
                else
                    echo "$(date): Ghost member $GHOST_IP is in desired state — may still be starting up, skipping"
                fi
            done <<< "$GHOST_MEMBERS"

            # Refresh member list after ghost cleanup
            RAW_MEMBER_LIST=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" member list 2>/dev/null || true)
        fi

        # ================================================================
        # CLUSTER ID MISMATCH DETECTION
        # Check if any peer has a different view of the cluster
        # ================================================================
        for DESIRED_IP in $DESIRED_IPS; do
            if [ "$DESIRED_IP" = "$MY_IP" ]; then
                continue
            fi
            PEER_ENDPOINT="${ETCD_PROTOCOL}://${DESIRED_IP}:${HOST_ETCD_CLIENT_PORT}"
            PEER_MEMBERS=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$PEER_ENDPOINT" --timeout=5s member list 2>/dev/null || true)
            if [ -n "$PEER_MEMBERS" ]; then
                # Check if the peer knows about us
                if ! echo "$PEER_MEMBERS" | grep -q "$MY_NAME"; then
                    echo "$(date): CLUSTER ID MISMATCH — peer $DESIRED_IP has a different cluster (does not know about $MY_NAME)"
                    echo "$(date): Peer's cluster members:"
                    echo "$PEER_MEMBERS"
                    echo "$(date): Restarting local etcd to trigger self-healing rejoin..."
                    # Wipe local data and restart — supervisord will restart us via start-etcd.sh
                    rm -rf /var/lib/etcd/*
                    supervisorctl restart etcd 2>/dev/null || true
                    # Sleep to let etcd restart before next cycle
                    sleep 60
                    break 2  # Break out of both the for loop and the if block
                fi
                # One reachable peer is enough to verify
                break
            fi
        done

        # ================================================================
        # PATRONI SYSTEM ID MISMATCH SELF-HEALING
        # Detects when the Patroni initialize key in etcd contains a
        # system ID that doesn't match local postgres data — this happens
        # when a brief failed bootstrap wrote a new system ID to etcd but
        # all nodes still have data from the original cluster.
        # Safe guard: only clears when no active Patroni leader exists.
        # ================================================================
        if [ -d /var/lib/postgresql/data/global ]; then
            LOCAL_SYS_ID=$(/usr/lib/postgresql/14/bin/pg_controldata /var/lib/postgresql/data 2>/dev/null \
                | grep "Database system identifier" | awk '{print $NF}' || true)
            ETCD_SYS_ID=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" \
                get /patroni/postgres-cluster/initialize 2>/dev/null || true)

            if [ -n "$LOCAL_SYS_ID" ] && [ -n "$ETCD_SYS_ID" ] && [ "$LOCAL_SYS_ID" != "$ETCD_SYS_ID" ]; then
                echo "$(date): PATRONI SYSTEM ID MISMATCH DETECTED"
                echo "  Local postgres system ID : $LOCAL_SYS_ID"
                echo "  etcd initialize key value: $ETCD_SYS_ID"

                # Only clear if no active Patroni leader is holding the lock
                PATRONI_LEADER=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" \
                    get /patroni/postgres-cluster/leader 2>/dev/null || true)
                if [ -z "$PATRONI_LEADER" ]; then
                    echo "$(date): No active Patroni leader — clearing stale Patroni cluster state to allow re-bootstrap..."
                    etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" \
                        rm --recursive /patroni/postgres-cluster/ 2>/dev/null || true
                    echo "$(date): Patroni cluster state cleared — nodes will re-bootstrap on next Patroni restart"
                else
                    # Active leader exists — this node's postgres data belongs to a different cluster.
                    # Wipe local data so Patroni re-clones from the primary instead of looping.
                    echo "$(date): Active leader ($PATRONI_LEADER) exists — wiping local postgres data for re-clone from primary..."
                    supervisorctl stop patroni 2>/dev/null || true
                    rm -rf /var/lib/postgresql/data/*
                    supervisorctl start patroni 2>/dev/null || true
                    echo "$(date): Local postgres data wiped — Patroni will re-clone from $PATRONI_LEADER"
                fi
            fi
        fi

        # ================================================================
        # STALE PATRONI LEADER CLEANUP
        # If the declared Patroni leader's REST API is unreachable and
        # we have local postgres data, clear the leader key to trigger
        # re-election. This unblocks fresh-install nodes that are
        # waiting for a data source but can't reach the stale leader.
        # Safe guards:
        #   - only runs when we have local postgres data (we are a
        #     viable leader candidate — won't create a blank cluster)
        #   - never clears our own leader key
        # ================================================================
        PATRONI_LEADER=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" \
            get /patroni/postgres-cluster/leader 2>/dev/null || true)
        if [ -n "$PATRONI_LEADER" ] && [ "$PATRONI_LEADER" != "$MY_NAME" ] && [ -d /var/lib/postgresql/data/global ]; then
            LEADER_IP=$(echo "$PATRONI_LEADER" | sed 's/^node-//' | tr '-' '.')
            PATRONI_SCHEME=$([ "$SSL_ENABLED" = "true" ] && echo "https" || echo "http")
            LEADER_API="${PATRONI_SCHEME}://${LEADER_IP}:${HOST_PATRONI_API_PORT}"
            if ! curl -s -k --max-time 5 "${LEADER_API}/health" >/dev/null 2>&1; then
                echo "$(date): Patroni leader $PATRONI_LEADER is unreachable at $LEADER_API"
                echo "$(date): Clearing stale leader key to trigger re-election (local data present)..."
                etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" \
                    rm /patroni/postgres-cluster/leader 2>/dev/null || true
                echo "$(date): Stale leader key cleared — Patroni will elect a new leader"
            fi
        fi

        # Parse v2 etcdctl output format: "id: name=... peerURLs=... clientURLs=https://IP:PORT isLeader=..."
        CURRENT_MEMBERS=$(echo "$RAW_MEMBER_LIST" | grep "clientURLs=" | sed 's/.*clientURLs=//' | sed 's/ .*//' | sed 's|https\?://||g' | sed "s|:${HOST_ETCD_CLIENT_PORT}||g" | grep -v "^$" | sort)

        echo "Parsed current etcd members (IPs only):"
        echo "$CURRENT_MEMBERS"
        echo "Number of current members: $(echo "$CURRENT_MEMBERS" | wc -l)"
    else
        echo "$(date): WARNING: Cannot connect to etcd at either endpoint"
        echo "Tried local: $LOCAL_ETCD_ENDPOINT"
        echo "Tried external: $EXTERNAL_ETCD_ENDPOINT"
        echo "This might be normal if etcd is still starting up"
        echo "Sleeping for 5 minutes (300 seconds)..."
        sleep 300
        continue
    fi

    if [ -n "$CURRENT_MEMBERS" ]; then
        echo "$(date): Processing cluster member differences..."

        # Find members to remove (in current but not in desired)
        MEMBERS_TO_REMOVE=""
        for CURRENT_IP in $CURRENT_MEMBERS; do
            if ! echo "$DESIRED_IPS" | grep -q "^$CURRENT_IP$"; then
                echo "$(date): Member $CURRENT_IP is NOT in desired state (should be removed)"
                MEMBERS_TO_REMOVE="$MEMBERS_TO_REMOVE $CURRENT_IP"
            else
                echo "$(date): Member $CURRENT_IP is in desired state (keeping)"
            fi
        done

        # Find new members (in desired but not in current)
        NEW_MEMBERS=""
        for DESIRED_IP in $DESIRED_IPS; do
            if ! echo "$CURRENT_MEMBERS" | grep -q "^$DESIRED_IP$"; then
                echo "$(date): Member $DESIRED_IP is NEW (will self-add when it starts)"
                NEW_MEMBERS="$NEW_MEMBERS $DESIRED_IP"
            fi
        done

        echo "Summary:"
        echo "  Members to keep: $(echo "$CURRENT_MEMBERS" | wc -l) - $(echo "$DESIRED_IPS" | wc -l) = $(echo "$MEMBERS_TO_REMOVE" | wc -w) will be removed"
        echo "  New members expected: $(echo "$NEW_MEMBERS" | wc -w)"

        # Process removals
        for CURRENT_IP in $MEMBERS_TO_REMOVE; do
            if [ -n "$CURRENT_IP" ]; then
                echo "$(date): Processing removal of member: $CURRENT_IP"

                # Get member ID from v2 etcdctl output
                echo "Looking for member with client URL containing: $CURRENT_IP:${HOST_ETCD_CLIENT_PORT}"
                MEMBER_ID=$(etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" member list 2>/dev/null | grep "clientURLs=.*${CURRENT_IP}:${HOST_ETCD_CLIENT_PORT}" | cut -d: -f1 | tr -d ' ' | head -n1)

                echo "Found member ID: $MEMBER_ID"

                if [ -n "$MEMBER_ID" ] && [ "$MEMBER_ID" != "null" ]; then
                    echo "$(date): Removing member $CURRENT_IP (ID: $MEMBER_ID) from etcd cluster..."

                    # Remove from etcd cluster
                    if etcdctl $ETCDCTL_SSL_OPTS --endpoints="$ETCD_ENDPOINT" member remove "$MEMBER_ID" 2>&1; then
                        echo "$(date): Successfully removed member $CURRENT_IP"
                    else
                        echo "$(date): Failed to remove member $CURRENT_IP, it may have already left"
                    fi
                else
                    echo "$(date): Could not find member ID for $CURRENT_IP"
                fi
            fi
        done

        # Note: We don't add new members here, as they will add themselves when they start up
        # This is the expected behavior for etcd clusters

        # Update the ETCD_INITIAL_CLUSTER in cluster_env to reflect current desired state
        echo "Updating ETCD_INITIAL_CLUSTER configuration..."
        NEW_ETCD_INITIAL_CLUSTER=""
        NEW_ETCD_HOSTS=""

        for IP in $DESIRED_IPS; do
            NODE_NAME="node-$(echo $IP | tr '.' '-')"
            if [ -n "$NEW_ETCD_INITIAL_CLUSTER" ]; then
                NEW_ETCD_INITIAL_CLUSTER="${NEW_ETCD_INITIAL_CLUSTER},"
                NEW_ETCD_HOSTS="${NEW_ETCD_HOSTS},"
            fi
            NEW_ETCD_INITIAL_CLUSTER="${NEW_ETCD_INITIAL_CLUSTER}${NODE_NAME}=${ETCD_PROTOCOL}://${IP}:${HOST_ETCD_PEER_PORT}"
            NEW_ETCD_HOSTS="${NEW_ETCD_HOSTS}${IP}:${HOST_ETCD_CLIENT_PORT}"
        done

        # Update cluster_env file with new configuration
        sed -i "s|^ETCD_INITIAL_CLUSTER=.*|ETCD_INITIAL_CLUSTER=$NEW_ETCD_INITIAL_CLUSTER|" /etc/cluster_env
        sed -i "s|^ETCD_HOSTS=.*|ETCD_HOSTS=$NEW_ETCD_HOSTS|" /etc/cluster_env
        echo "Updated ETCD_INITIAL_CLUSTER to: $NEW_ETCD_INITIAL_CLUSTER"

    else
        echo "$(date): No current etcd members found or connection failed"
    fi

    echo "================================================================================"
    echo "$(date): Cluster update cycle complete, sleeping for 5 minutes (300 seconds)..."
    echo "================================================================================"
    sleep 300
done
