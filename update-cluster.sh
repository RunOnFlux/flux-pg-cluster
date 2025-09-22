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
    LOCAL_ETCD_ENDPOINT="http://127.0.0.1:${ETCD_CLIENT_PORT}"
    # For external connections to other nodes, use external IP and host port
    EXTERNAL_ETCD_ENDPOINT="http://${MY_IP}:${HOST_ETCD_CLIENT_PORT}"

    echo "Local etcd endpoint: $LOCAL_ETCD_ENDPOINT"
    echo "External etcd endpoint: $EXTERNAL_ETCD_ENDPOINT"
    echo "Testing etcd connection to local endpoint first..."

    # Test etcd connectivity first (try local endpoint)
    ETCD_ENDPOINT=""
    if etcdctl --endpoints="$LOCAL_ETCD_ENDPOINT" member list >/dev/null 2>&1; then
        echo "etcd connection successful via local endpoint"
        ETCD_ENDPOINT="$LOCAL_ETCD_ENDPOINT"
    elif etcdctl --endpoints="$EXTERNAL_ETCD_ENDPOINT" member list >/dev/null 2>&1; then
        echo "etcd connection successful via external endpoint"
        ETCD_ENDPOINT="$EXTERNAL_ETCD_ENDPOINT"
    fi

    if [ -n "$ETCD_ENDPOINT" ]; then
        echo "Using etcd endpoint: $ETCD_ENDPOINT"

        # Get detailed member list
        echo "Raw etcd member list:"
        etcdctl --endpoints="$ETCD_ENDPOINT" member list --write-out=json 2>&1 || echo "Failed to get JSON output"

        CURRENT_MEMBERS=$(etcdctl --endpoints="$ETCD_ENDPOINT" member list --write-out=json 2>/dev/null | jq -r '.members[] | select(.clientURLs | length > 0) | .clientURLs[0]' | sed 's|http://||g' | sed "s|:${HOST_ETCD_CLIENT_PORT}||g" | sort)

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

                # Get member ID
                echo "Looking for member with client URL containing: $CURRENT_IP:${HOST_ETCD_CLIENT_PORT}"
                MEMBER_ID=$(etcdctl --endpoints="$ETCD_ENDPOINT" member list --write-out=json 2>/dev/null | jq -r ".members[] | select(.clientURLs[0] | contains(\"$CURRENT_IP:${HOST_ETCD_CLIENT_PORT}\")) | .ID" | head -n1)

                echo "Found member ID: $MEMBER_ID"

                if [ -n "$MEMBER_ID" ] && [ "$MEMBER_ID" != "null" ]; then
                    echo "$(date): Removing member $CURRENT_IP (ID: $MEMBER_ID) from etcd cluster..."

                    # Remove from etcd cluster
                    if etcdctl --endpoints="$ETCD_ENDPOINT" member remove "$MEMBER_ID" 2>&1; then
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
            NEW_ETCD_INITIAL_CLUSTER="${NEW_ETCD_INITIAL_CLUSTER}${NODE_NAME}=http://${IP}:${HOST_ETCD_PEER_PORT}"
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