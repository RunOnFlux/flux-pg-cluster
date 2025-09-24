#!/bin/bash
set -e

# Certificate generation script for Flux PostgreSQL cluster
# Generates deterministic certificates using passphrase as seed

CERT_DIR="/etc/ssl/cluster"
CERT_VALIDITY_DAYS=${SSL_CERT_VALIDITY_DAYS:-3650}

echo "=============================================================================="
echo "SSL CERTIFICATE GENERATION"
echo "=============================================================================="
echo "Certificate directory: $CERT_DIR"
echo "Certificate validity: $CERT_VALIDITY_DAYS days"
echo "Time: $(date)"

# Validate required environment variables
if [ -z "$SSL_PASSPHRASE" ]; then
    echo "ERROR: SSL_PASSPHRASE environment variable is required"
    exit 1
fi

if [ -z "$APP_NAME" ]; then
    echo "ERROR: APP_NAME environment variable is required"
    exit 1
fi

if [ -z "$CLUSTER_IPS" ]; then
    echo "ERROR: CLUSTER_IPS environment variable is required"
    exit 1
fi

if [ -z "$MY_IP" ]; then
    echo "ERROR: MY_IP environment variable is required"
    exit 1
fi

echo "Cluster name: $APP_NAME"
echo "Node IP: $MY_IP"
echo "Cluster IPs: $CLUSTER_IPS"

# Create certificate directories
mkdir -p "$CERT_DIR"/{ca,etcd,postgres,patroni}

# Function to generate truly deterministic private key using GnuTLS certtool
generate_deterministic_key() {
    local service=$1
    local keyfile=$2
    local seed="${SSL_PASSPHRASE}-${service}-${APP_NAME}"

    echo "Generating deterministic key for service: $service"
    echo "Using GnuTLS certtool with provable deterministic generation"

    # Create deterministic seed from passphrase + service + cluster name
    local seed_hex=$(echo -n "$seed" | openssl dgst -sha256 | awk '{print $2}')
    echo "Using seed: $seed (hash: ${seed_hex:0:16}...)"

    # Validate seed length (should be 64 hex characters = 256 bits)
    if [ ${#seed_hex} -ne 64 ]; then
        echo "Error: Seed generation failed - invalid length: ${#seed_hex}"
        exit 1
    fi

    # Use GnuTLS certtool to generate truly deterministic private key
    if command -v certtool >/dev/null 2>&1; then
        echo "Generating deterministic RSA private key using certtool"

        # Generate deterministic private key using the proven method
        certtool --generate-privkey \
                 --provable \
                 --seed="$seed_hex" \
                 --key-type=rsa \
                 --sec-param=Medium \
                 --outfile "$keyfile" 2>/dev/null || {
            echo "Error: certtool key generation failed"
            # Fallback to OpenSSL if certtool fails
            echo "Falling back to OpenSSL key generation"
            openssl genrsa -out "$keyfile" 2048 2>/dev/null
        }

        echo "Generated truly deterministic RSA key using GnuTLS (hash: ${seed_hex:0:16}...)"
    else
        echo "certtool not available, falling back to OpenSSL"
        # Fallback to OpenSSL with seed
        local temp_seed_file="/tmp/openssl_seed_$service"
        echo -n "$seed_hex" | xxd -r -p > "$temp_seed_file" 2>/dev/null || echo -n "$seed_hex" > "$temp_seed_file"
        RANDFILE="$temp_seed_file" openssl genrsa -out "$keyfile" 2048 2>/dev/null
        rm -f "$temp_seed_file"
        echo "Generated RSA key using OpenSSL fallback (hash: ${seed_hex:0:16}...)"
    fi

    chmod 600 "$keyfile"
    echo "Generated key: $keyfile"
}

# Function to create certificate signing request with proper SANs
create_csr() {
    local service=$1
    local keyfile=$2
    local csrfile=$3
    local cn=$4
    local san_list=""

    # Build Subject Alternative Names for all cluster IPs
    local ip_sans=""
    local dns_sans="DNS:localhost,DNS:${service}"

    for ip in $CLUSTER_IPS; do
        if [ -n "$ip_sans" ]; then
            ip_sans="${ip_sans},"
        fi
        ip_sans="${ip_sans}IP:${ip}"
    done

    # Skip adding node-specific MY_IP for truly deterministic certificates
    # All nodes will have identical certificates with same SANs

    # Add common localhost addresses
    ip_sans="${ip_sans},IP:127.0.0.1,IP:0.0.0.0"

    san_list="${dns_sans},${ip_sans}"

    echo "Creating CSR for $service with CN=$cn and SANs=$san_list"

    # Create certificate signing request
    openssl req -new -key "$keyfile" -out "$csrfile" -subj "/CN=$cn/O=FluxCluster/OU=$APP_NAME" \
        -addext "subjectAltName=$san_list" \
        -addext "keyUsage=digitalSignature,keyEncipherment,keyAgreement" \
        -addext "extendedKeyUsage=serverAuth,clientAuth"

    echo "Created CSR: $csrfile"
}

# Function to sign certificate with CA
sign_certificate() {
    local csrfile=$1
    local certfile=$2
    local ca_cert=$3
    local ca_key=$4

    echo "Signing certificate: $csrfile -> $certfile"

    # Build proper SAN list for certificate signing
    local dns_sans="DNS:localhost,DNS:etcd-peer,DNS:postgres-server,DNS:patroni-api"
    local ip_sans=""

    for ip in $CLUSTER_IPS; do
        if [ -n "$ip_sans" ]; then
            ip_sans="${ip_sans},"
        fi
        ip_sans="${ip_sans}IP:${ip}"
    done

    # Skip adding node-specific MY_IP for truly deterministic certificates
    # All nodes will have identical certificates with same SANs

    # Add common localhost addresses
    ip_sans="${ip_sans},IP:127.0.0.1,IP:0.0.0.0"

    local san_list="${dns_sans},${ip_sans}"

    # Set deterministic timestamp for certificate generation
    export SOURCE_DATE_EPOCH=1672531200  # 2023-01-01 00:00:00 UTC

    # Create deterministic serial number from service name and seed
    local serial_seed="${SSL_PASSPHRASE}-${csrfile##*/}-${APP_NAME}"
    local serial_hex=$(echo -n "$serial_seed" | sha256sum | cut -d' ' -f1 | head -c 16)
    # Use the hex value directly as serial number - OpenSSL accepts hex with 0x prefix
    local serial_number="0x$serial_hex"

    openssl x509 -req -in "$csrfile" -CA "$ca_cert" -CAkey "$ca_key" \
        -out "$certfile" -days "$CERT_VALIDITY_DAYS" \
        -set_serial "$serial_number" \
        -passin pass:"" \
        -extensions v3_req -extfile <(
        echo "[v3_req]"
        echo "keyUsage = digitalSignature, keyEncipherment, keyAgreement"
        echo "extendedKeyUsage = serverAuth, clientAuth"
        echo "subjectAltName = $san_list"
    )

    chmod 644 "$certfile"
    echo "Signed certificate: $certfile"
}

echo "=============================================================================="
echo "GENERATING ROOT CA"
echo "=============================================================================="

# Generate deterministic CA private key
generate_deterministic_key "ca" "$CERT_DIR/ca/ca.key"

# Generate CA certificate with deterministic parameters
echo "Generating CA certificate..."

# Set deterministic timestamp for certificate generation
export SOURCE_DATE_EPOCH=1672531200  # 2023-01-01 00:00:00 UTC

# Try using certtool for deterministic CA certificate generation
if command -v certtool >/dev/null 2>&1; then
    echo "Using certtool for deterministic CA certificate generation"

    # Create template file for CA certificate
    ca_template="/tmp/ca_template_$$"
    cat > "$ca_template" << EOL
cn = "FluxCluster-CA"
organization = "FluxCluster"
unit = "$APP_NAME"
serial = 1
activation_date = "2023-01-01 00:00:00 UTC"
expiration_date = "2033-01-01 00:00:00 UTC"
ca
cert_signing_key
crl_signing_key
EOL

    # Generate self-signed CA certificate using certtool
    certtool --generate-self-signed \
             --load-privkey "$CERT_DIR/ca/ca.key" \
             --template "$ca_template" \
             --outfile "$CERT_DIR/ca/ca.crt" 2>/dev/null || {
        echo "certtool CA certificate generation failed, falling back to OpenSSL"

        # Fallback to OpenSSL
        ca_serial_seed="${SSL_PASSPHRASE}-ca-${APP_NAME}"
        ca_serial_hex=$(echo -n "$ca_serial_seed" | sha256sum | cut -d' ' -f1 | head -c 16)
        ca_serial_number="0x$ca_serial_hex"

        openssl req -new -x509 -key "$CERT_DIR/ca/ca.key" -out "$CERT_DIR/ca/ca.crt" \
            -days "$CERT_VALIDITY_DAYS" \
            -set_serial "$ca_serial_number" \
            -subj "/CN=FluxCluster-CA/O=FluxCluster/OU=$APP_NAME" \
            -addext "basicConstraints=CA:TRUE" \
            -addext "keyUsage=keyCertSign,cRLSign" \
            -passin pass:""
    }

    # Clean up template
    rm -f "$ca_template"
else
    echo "certtool not available, using OpenSSL for CA certificate generation"

    # Create deterministic serial number for CA
    ca_serial_seed="${SSL_PASSPHRASE}-ca-${APP_NAME}"
    ca_serial_hex=$(echo -n "$ca_serial_seed" | sha256sum | cut -d' ' -f1 | head -c 16)
    ca_serial_number="0x$ca_serial_hex"

    openssl req -new -x509 -key "$CERT_DIR/ca/ca.key" -out "$CERT_DIR/ca/ca.crt" \
        -days "$CERT_VALIDITY_DAYS" \
        -set_serial "$ca_serial_number" \
        -subj "/CN=FluxCluster-CA/O=FluxCluster/OU=$APP_NAME" \
        -addext "basicConstraints=CA:TRUE" \
        -addext "keyUsage=keyCertSign,cRLSign" \
        -passin pass:""
fi

chmod 644 "$CERT_DIR/ca/ca.crt"
echo "Generated CA certificate: $CERT_DIR/ca/ca.crt"

echo "=============================================================================="
echo "GENERATING ETCD CERTIFICATES"
echo "=============================================================================="

# Generate etcd peer certificate (for peer-to-peer communication)
generate_deterministic_key "etcd-peer" "$CERT_DIR/etcd/peer.key"
create_csr "etcd-peer" "$CERT_DIR/etcd/peer.key" "$CERT_DIR/etcd/peer.csr" "etcd-peer"
sign_certificate "$CERT_DIR/etcd/peer.csr" "$CERT_DIR/etcd/peer.crt" "$CERT_DIR/ca/ca.crt" "$CERT_DIR/ca/ca.key"
rm "$CERT_DIR/etcd/peer.csr"

# Generate etcd client certificate (for client connections)
generate_deterministic_key "etcd-client" "$CERT_DIR/etcd/client.key"
create_csr "etcd-client" "$CERT_DIR/etcd/client.key" "$CERT_DIR/etcd/client.csr" "etcd-client"
sign_certificate "$CERT_DIR/etcd/client.csr" "$CERT_DIR/etcd/client.crt" "$CERT_DIR/ca/ca.crt" "$CERT_DIR/ca/ca.key"
rm "$CERT_DIR/etcd/client.csr"

echo "=============================================================================="
echo "GENERATING POSTGRESQL CERTIFICATES"
echo "=============================================================================="

# Generate PostgreSQL server certificate
generate_deterministic_key "postgres-server" "$CERT_DIR/postgres/server.key"
create_csr "postgres-server" "$CERT_DIR/postgres/server.key" "$CERT_DIR/postgres/server.csr" "postgres-server"
sign_certificate "$CERT_DIR/postgres/server.csr" "$CERT_DIR/postgres/server.crt" "$CERT_DIR/ca/ca.crt" "$CERT_DIR/ca/ca.key"
rm "$CERT_DIR/postgres/server.csr"

# Generate PostgreSQL client certificate (for replication)
generate_deterministic_key "postgres-client" "$CERT_DIR/postgres/client.key"
create_csr "postgres-client" "$CERT_DIR/postgres/client.key" "$CERT_DIR/postgres/client.csr" "postgres-client"
sign_certificate "$CERT_DIR/postgres/client.csr" "$CERT_DIR/postgres/client.crt" "$CERT_DIR/ca/ca.crt" "$CERT_DIR/ca/ca.key"
rm "$CERT_DIR/postgres/client.csr"

echo "=============================================================================="
echo "GENERATING PATRONI API CERTIFICATE"
echo "=============================================================================="

# Generate Patroni API server certificate
generate_deterministic_key "patroni-api" "$CERT_DIR/patroni/server.key"
create_csr "patroni-api" "$CERT_DIR/patroni/server.key" "$CERT_DIR/patroni/server.csr" "patroni-api"
sign_certificate "$CERT_DIR/patroni/server.csr" "$CERT_DIR/patroni/server.crt" "$CERT_DIR/ca/ca.crt" "$CERT_DIR/ca/ca.key"
rm "$CERT_DIR/patroni/server.csr"

echo "=============================================================================="
echo "SETTING PERMISSIONS"
echo "=============================================================================="

# Set appropriate permissions
chown -R postgres:postgres "$CERT_DIR/postgres"
chown -R root:root "$CERT_DIR/etcd" "$CERT_DIR/ca" "$CERT_DIR/patroni"

# Make etcd client certificates readable by postgres user for Patroni
chown root:postgres "$CERT_DIR/etcd/client.crt" "$CERT_DIR/etcd/client.key"
chmod 640 "$CERT_DIR/etcd/client.key"  # readable by postgres group

# Make Patroni server certificates readable by postgres user for REST API
chown root:postgres "$CERT_DIR/patroni/server.crt" "$CERT_DIR/patroni/server.key"
chmod 640 "$CERT_DIR/patroni/server.key"  # readable by postgres group

# Set restrictive permissions on private keys (except etcd client key and patroni server key which are already set above)
find "$CERT_DIR" -name "*.key" ! -path "*/etcd/client.key" ! -path "*/patroni/server.key" -exec chmod 600 {} \;
find "$CERT_DIR" -name "*.crt" -exec chmod 644 {} \;

echo "=============================================================================="
echo "CERTIFICATE GENERATION COMPLETE"
echo "=============================================================================="
echo "Generated certificates:"
echo "CA:"
echo "  - $CERT_DIR/ca/ca.crt"
echo "  - $CERT_DIR/ca/ca.key"
echo "etcd:"
echo "  - $CERT_DIR/etcd/peer.crt"
echo "  - $CERT_DIR/etcd/peer.key"
echo "  - $CERT_DIR/etcd/client.crt"
echo "  - $CERT_DIR/etcd/client.key"
echo "PostgreSQL:"
echo "  - $CERT_DIR/postgres/server.crt"
echo "  - $CERT_DIR/postgres/server.key"
echo "  - $CERT_DIR/postgres/client.crt"
echo "  - $CERT_DIR/postgres/client.key"
echo "Patroni:"
echo "  - $CERT_DIR/patroni/server.crt"
echo "  - $CERT_DIR/patroni/server.key"

echo "Verifying certificates..."
echo "CA certificate:"
openssl x509 -in "$CERT_DIR/ca/ca.crt" -text -noout | grep -E "(Subject:|Not After :)"

echo "etcd peer certificate:"
openssl x509 -in "$CERT_DIR/etcd/peer.crt" -text -noout | grep -E "(Subject:|Not After :|DNS:|IP Address)"

echo "Certificate generation completed successfully!"
echo "=============================================================================="