#!/bin/bash

echo "================================================================================"
echo "PATRONI CLUSTER REPAIR SCRIPT"
echo "================================================================================"
echo "This script will fix the PostgreSQL system ID mismatch issue"
echo "Time: $(date)"
echo ""

echo "1. STOPPING PATRONI..."
supervisorctl stop patroni
sleep 3

echo "2. CHECKING CURRENT POSTGRESQL DATA..."
echo "Current data directory contents:"
ls -la /var/lib/postgresql/data/ 2>/dev/null | head -5
echo ""

echo "Current PG_VERSION (if exists):"
cat /var/lib/postgresql/data/PG_VERSION 2>/dev/null || echo "No PG_VERSION found"
echo ""

echo "3. BACKING UP CURRENT DATA..."
if [ -d "/var/lib/postgresql/data" ] && [ "$(ls -A /var/lib/postgresql/data)" ]; then
    echo "Creating backup of existing data..."
    mv /var/lib/postgresql/data /var/lib/postgresql/data.backup.$(date +%Y%m%d_%H%M%S) 2>/dev/null || echo "Backup failed, but continuing..."
else
    echo "No data to backup"
fi

echo "4. REMOVING OLD POSTGRESQL DATA COMPLETELY..."
rm -rf /var/lib/postgresql/data/*
rm -rf /var/lib/postgresql/data/.* 2>/dev/null || true

echo "5. RECREATING CLEAN DATA DIRECTORY..."
mkdir -p /var/lib/postgresql/data
chown postgres:postgres /var/lib/postgresql/data
chmod 700 /var/lib/postgresql/data

echo "6. VERIFYING CLEAN STATE..."
echo "Data directory is now:"
ls -la /var/lib/postgresql/data/ || echo "Directory empty (good!)"
echo ""

echo "7. RESTARTING PATRONI..."
echo "Patroni will now initialize a fresh PostgreSQL instance..."
supervisorctl start patroni

echo ""
echo "8. WAITING FOR PATRONI TO INITIALIZE..."
echo "This may take 1-2 minutes. Checking every 10 seconds..."

for i in {1..12}; do
    sleep 10
    echo "Check $i/12: $(date)"

    # Check if Patroni process is stable
    if pgrep -f "python3 -m patroni" >/dev/null; then
        echo "  - Patroni process is running"
    else
        echo "  - Patroni process not found"
        continue
    fi

    # Check if PostgreSQL is running
    if pgrep -f "postgres.*main" >/dev/null; then
        echo "  - PostgreSQL process is running"

        # Check if API is responding
        if curl -s localhost:8008/ >/dev/null 2>&1; then
            echo "  - Patroni API is responding"
            echo ""
            echo "SUCCESS! Cluster appears to be working."
            echo "Running final status check..."
            patronictl -c /etc/patroni/patroni.yml list 2>/dev/null || echo "patronictl not ready yet"
            exit 0
        else
            echo "  - Patroni API not ready yet"
        fi
    else
        echo "  - PostgreSQL not running yet"
    fi
done

echo ""
echo "Initialization is taking longer than expected."
echo "Check the logs for any issues:"
echo "  tail -20 /var/log/supervisor/patroni.err.log"
echo "  tail -20 /var/log/supervisor/patroni.out.log"
echo ""
echo "Current supervisor status:"
supervisorctl status