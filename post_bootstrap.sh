#!/bin/bash
set -e

# Called by Patroni after cluster bootstrap (bootstrap.post_bootstrap)
# First argument is the superuser connection string
CONN="${1}"

echo "post_bootstrap: Running post-bootstrap configuration..."
echo "post_bootstrap: POSTGRES_DB=${POSTGRES_DB:-postgres}"

# Escape password for SQL single-quote literal (' -> '')
# Passwords containing '$' are already rejected by entrypoint.sh validation
ESCAPED_PWD=$(printf '%s' "${POSTGRES_SUPERUSER_PASSWORD}" | sed "s/'/''/g")

# Create admin role (idempotent) and set password
PGPASSWORD="${POSTGRES_SUPERUSER_PASSWORD}" psql "${CONN}" -c "
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'admin') THEN
    CREATE ROLE admin WITH LOGIN CREATEROLE CREATEDB;
  END IF;
END \$\$;
ALTER ROLE admin WITH PASSWORD '${ESCAPED_PWD}';
"

echo "post_bootstrap: admin role ready"

# Create application database if different from the built-in 'postgres'
if [ "${POSTGRES_DB:-postgres}" != "postgres" ]; then
    echo "post_bootstrap: Creating database '${POSTGRES_DB}'..."
    PGPASSWORD="${POSTGRES_SUPERUSER_PASSWORD}" psql "${CONN}" \
        -c "CREATE DATABASE \"${POSTGRES_DB}\" OWNER admin;" 2>/dev/null \
        || echo "post_bootstrap: Database '${POSTGRES_DB}' already exists, skipping."
fi

echo "post_bootstrap: Complete"
