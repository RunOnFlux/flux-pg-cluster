#!/bin/bash

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
curl -s localhost:2379/health && echo "etcd OK" || echo "etcd FAILED"
echo ""

echo "5. PATRONI STATUS:"
echo "Patroni process check:"
pgrep -f "patroni" >/dev/null && echo "Patroni process is running" || echo "Patroni process NOT running"
echo ""

echo "Patroni REST API detailed test (port 8008):"
PATRONI_RESPONSE=$(curl -s -w "HTTP_CODE:%{http_code}" localhost:8008/ 2>/dev/null)
if [[ "$PATRONI_RESPONSE" == *"HTTP_CODE:200"* ]]; then
    echo "Patroni API OK - Response: ${PATRONI_RESPONSE%HTTP_CODE:*}"
elif [[ "$PATRONI_RESPONSE" == *"HTTP_CODE:"* ]]; then
    echo "Patroni API responded but with error code: ${PATRONI_RESPONSE##*HTTP_CODE:}"
else
    echo "Patroni API connection failed - no response"
fi
echo ""

echo "Testing direct port connectivity:"
timeout 3 bash -c 'cat < /dev/null > /dev/tcp/localhost/8008' 2>/dev/null && echo "Port 8008 is open" || echo "Port 8008 is not accessible"
echo ""

echo "Patroni configuration check:"
echo "Generated patroni.yml restapi section:"
grep -A 5 "restapi:" /etc/patroni/patroni.yml 2>/dev/null || echo "Cannot read patroni.yml"
echo ""

echo "Patroni cluster status (if API works):"
curl -s localhost:8008/cluster | jq '.' 2>/dev/null || echo "Patroni cluster API call failed"
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