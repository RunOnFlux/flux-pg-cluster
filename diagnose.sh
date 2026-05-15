#!/bin/bash

# Source cluster environment to detect SSL settings and ports
source /etc/cluster_env 2>/dev/null || true

# Build SSL-aware curl flags for etcd and Patroni
if [ "$SSL_ENABLED" = "true" ]; then
    ETCD_CURL_OPTS="--cacert /etc/ssl/cluster/ca/ca.crt --cert /etc/ssl/cluster/etcd/client.crt --key /etc/ssl/cluster/etcd/client.key"
    PATRONI_CURL_OPTS="-k"
    ETCD_SCHEME="https"
    PATRONI_SCHEME="https"
else
    ETCD_CURL_OPTS=""
    PATRONI_CURL_OPTS=""
    ETCD_SCHEME="http"
    PATRONI_SCHEME="http"
fi

ETCD_PORT=${HOST_ETCD_CLIENT_PORT:-2379}
PATRONI_PORT=${HOST_PATRONI_API_PORT:-8008}

echo "================================================================================"
echo "PATRONI CLUSTER DIAGNOSTICS"
echo "================================================================================"
echo "Time: $(date)"
echo ""

echo "1. SUPERVISOR STATUS:"
supervisorctl status
echo ""

echo "2. POSTGRESQL PROCESS STATUS:"
ps aux | grep postgres | grep -v grep || echo "No PostgreSQL processes running"
echo ""

echo "3. LISTENING PORTS:"
echo "All relevant ports (netstat):"
netstat -tlnp | grep -E "(5432|8008|2379|2380)" || echo "No expected ports found with netstat"
echo ""
echo "Alternative port check (ss):"
ss -tlnp | grep -E "(5432|8008|2379|2380)" 2>/dev/null || echo "ss not available or no ports found"
echo ""
echo "All listening ports summary:"
netstat -tlnp | head -10 || echo "Cannot list ports"
echo ""

echo "4. ETCD STATUS:"
echo "etcd connection test:"
curl -sf $ETCD_CURL_OPTS "${ETCD_SCHEME}://localhost:${ETCD_PORT}/health" && echo "etcd OK" || echo "etcd FAILED"
echo ""

echo "5. PATRONI STATUS:"
echo "Patroni process check:"
pgrep -f "patroni" >/dev/null && echo "Patroni process is running" || echo "Patroni process NOT running"
echo ""

echo "Patroni REST API detailed test (port ${PATRONI_PORT}):"
PATRONI_RESPONSE=$(curl -s $PATRONI_CURL_OPTS -w "HTTP_CODE:%{http_code}" "${PATRONI_SCHEME}://localhost:${PATRONI_PORT}/" 2>/dev/null)
if [[ "$PATRONI_RESPONSE" == *"HTTP_CODE:200"* ]]; then
    echo "Patroni API OK - Response: ${PATRONI_RESPONSE%HTTP_CODE:*}"
elif [[ "$PATRONI_RESPONSE" == *"HTTP_CODE:"* ]]; then
    echo "Patroni API responded but with error code: ${PATRONI_RESPONSE##*HTTP_CODE:}"
else
    echo "Patroni API connection failed - no response"
fi
echo ""

echo "Testing direct port connectivity:"
timeout 3 bash -c "cat < /dev/null > /dev/tcp/localhost/${PATRONI_PORT}" 2>/dev/null && echo "Port ${PATRONI_PORT} is open" || echo "Port ${PATRONI_PORT} is not accessible"
echo ""

echo "Patroni configuration check:"
echo "Generated patroni.yml restapi section:"
grep -A 5 "restapi:" /etc/patroni/patroni.yml 2>/dev/null || echo "Cannot read patroni.yml"
echo ""

echo "Patroni cluster status (if API works):"
curl -s $PATRONI_CURL_OPTS "${PATRONI_SCHEME}://localhost:${PATRONI_PORT}/cluster" | jq '.' 2>/dev/null || echo "Patroni cluster API call failed"
echo ""

echo "6. PATRONI CLUSTER LIST:"
patronictl -c /etc/patroni/patroni.yml list 2>/dev/null || echo "patronictl failed"
echo ""

echo "7. POSTGRESQL DATA DIRECTORY:"
echo "Contents of /var/lib/postgresql/data:"
ls -la /var/lib/postgresql/data/ 2>/dev/null | head -10 || echo "Directory not accessible"
echo ""

echo "8. RECENT LOGS (last 20 lines each):"
echo "--- PATRONI OUTPUT ---"
tail -20 /var/log/supervisor/patroni.out.log 2>/dev/null || echo "No patroni output log"
echo ""
echo "--- PATRONI ERRORS ---"
tail -20 /var/log/supervisor/patroni.err.log 2>/dev/null || echo "No patroni error log"
echo ""
echo "--- ETCD OUTPUT ---"
tail -10 /var/log/supervisor/etcd.out.log 2>/dev/null || echo "No etcd output log"
echo ""
echo "--- ETCD ERRORS ---"
tail -10 /var/log/supervisor/etcd.err.log 2>/dev/null || echo "No etcd error log"
echo ""

echo "9. DISK SPACE:"
df -h /var/lib/postgresql/data /var/lib/etcd 2>/dev/null || echo "Cannot check disk space"
echo ""

echo "10. PERMISSIONS CHECK:"
echo "PostgreSQL data directory permissions:"
ls -ld /var/lib/postgresql/data 2>/dev/null || echo "Cannot check permissions"
echo "etcd data directory permissions:"
ls -ld /var/lib/etcd 2>/dev/null || echo "Cannot check permissions"
echo ""

echo "================================================================================"
echo "DIAGNOSIS COMPLETE"
echo "================================================================================"