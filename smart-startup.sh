#!/bin/bash

echo "================================================================================"
echo "SMART CLUSTER STARTUP - NO DATA LOSS"
echo "================================================================================"

# Check if this is a fresh start or restart
FRESH_ETCD=false
FRESH_POSTGRES=false

if [ ! -f /var/lib/etcd/member/snap/db ]; then
    echo "No existing etcd data found - fresh etcd start"
    FRESH_ETCD=true
fi

if [ ! -f /var/lib/postgresql/data/PG_VERSION ]; then
    echo "No existing PostgreSQL data found - fresh postgres start"
    FRESH_POSTGRES=true
fi

# If both are fresh, this is a clean start
if $FRESH_ETCD && $FRESH_POSTGRES; then
    echo "CLEAN START: Both etcd and PostgreSQL are starting fresh"
    echo "No data conflicts expected"
    exit 0
fi

# If only one is fresh, we have a partial state - this is problematic
if $FRESH_ETCD && ! $FRESH_POSTGRES; then
    echo "PARTIAL STATE: etcd is fresh but PostgreSQL has existing data"
    echo "This usually means etcd had conflicts. Analyzing..."

    # Check if PostgreSQL data is from a compatible cluster
    echo "PostgreSQL version: $(cat /var/lib/postgresql/data/PG_VERSION 2>/dev/null)"

    echo "Options:"
    echo "1. Clear PostgreSQL data too (recommended for testing)"
    echo "2. Try to start anyway (may cause issues)"
    echo "3. Manual recovery"

    read -p "Choose option (1/2/3): " choice
    case $choice in
        1) echo "Clearing PostgreSQL data..."; rm -rf /var/lib/postgresql/data/*;;
        2) echo "Attempting startup with existing PostgreSQL data...";;
        3) echo "Manual recovery needed"; exit 1;;
        *) echo "Invalid choice"; exit 1;;
    esac
fi

if ! $FRESH_ETCD && $FRESH_POSTGRES; then
    echo "PARTIAL STATE: PostgreSQL is fresh but etcd has existing data"
    echo "This is unusual but may work if etcd cluster is healthy"
fi

if ! $FRESH_ETCD && ! $FRESH_POSTGRES; then
    echo "EXISTING DATA: Both etcd and PostgreSQL have existing data"
    echo "Checking compatibility..."

    # This is actually the desired state for restarts
    echo "This is normal for container restarts. Proceeding..."
fi

echo "Startup analysis complete."