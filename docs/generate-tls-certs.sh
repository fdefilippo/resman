#!/bin/bash
#
# Generate TLS certificates for ResMan.
# Usage: ./generate-tls-certs.sh [directory]
#
# This script generates:
# - a private certificate authority (CA)
# - a server certificate and private key
# - an optional client certificate for mTLS
#

set -e

CERT_DIR="${1:-/etc/resman/tls}"
VALIDITY_DAYS=365
COUNTRY="IT"
STATE="Italy"
LOCALITY="Rome"
ORG="ResMan"
ORG_UNIT="Monitoring"
CN_SERVER="resman.local"
CN_CLIENT="prometheus"

echo "=========================================="
echo "ResMan - TLS Certificate Generator"
echo "=========================================="
echo ""
echo "Certificate directory: $CERT_DIR"
echo "Validity: $VALIDITY_DAYS days"
echo ""

# Create the certificate directory.
sudo mkdir -p "$CERT_DIR"
cd "$CERT_DIR"

# 1. Generate the private CA.
echo "[1/5] Generating CA private key and certificate..."
sudo openssl genrsa -out ca.key 4096

sudo openssl req -x509 -new -nodes -sha256 -days $VALIDITY_DAYS \
    -key ca.key \
    -out ca.crt \
    -subj "/C=$COUNTRY/ST=$STATE/L=$LOCALITY/O=$ORG/OU=$ORG_UNIT/CN=ResMan CA"

echo "      CA certificate: ca.crt"
echo "      CA private key: ca.key"
echo ""

# 2. Generate the server private key.
echo "[2/5] Generating server private key..."
sudo openssl genrsa -out server.key 2048

# 3. Generate the server certificate signing request.
echo "[3/5] Generating server CSR..."
sudo openssl req -new -sha256 \
    -key server.key \
    -out server.csr \
    -subj "/C=$COUNTRY/ST=$STATE/L=$LOCALITY/O=$ORG/OU=$ORG_UNIT/CN=$CN_SERVER"

# 4. Sign the server certificate with the CA.
echo "[4/5] Signing server certificate with CA..."

# Create the Subject Alternative Name extension file.
cat > server_ext.cnf << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = $CN_SERVER
DNS.2 = localhost
DNS.3 = resman
IP.1 = 127.0.0.1
IP.2 = 192.168.1.1
EOF

sudo openssl x509 -req -sha256 -days $VALIDITY_DAYS \
    -in server.csr \
    -CA ca.crt \
    -CAkey ca.key \
    -CAcreateserial \
    -out server.crt \
    -extfile server_ext.cnf

echo "      Server certificate: server.crt"
echo "      Server private key: server.key"
echo ""

# 5. Generate the optional mTLS client certificate.
echo "[5/5] Generating client certificate (for mTLS)..."
sudo openssl genrsa -out client.key 2048

sudo openssl req -new -sha256 \
    -key client.key \
    -out client.csr \
    -subj "/C=$COUNTRY/ST=$STATE/L=$LOCALITY/O=$ORG/OU=$ORG_UNIT/CN=$CN_CLIENT"

# Create the client extension file.
cat > client_ext.cnf << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
extendedKeyUsage = clientAuth
EOF

sudo openssl x509 -req -sha256 -days $VALIDITY_DAYS \
    -in client.csr \
    -CA ca.crt \
    -CAkey ca.key \
    -CAcreateserial \
    -out client.crt \
    -extfile client_ext.cnf

echo "      Client certificate: client.crt"
echo "      Client private key: client.key"
echo ""

# Apply restrictive key permissions.
echo "Setting file permissions..."
sudo chmod 644 ca.crt server.crt client.crt
sudo chmod 600 ca.key server.key client.key
sudo chown -R root:root "$CERT_DIR"

# Remove intermediate signing files.
sudo rm -f server.csr client.csr server_ext.cnf client_ext.cnf ca.srl

echo ""
echo "=========================================="
echo "Certificates generated successfully!"
echo "=========================================="
echo ""
echo "Server Configuration (/etc/resman/resman.conf):"
echo "----------------------------------------------"
echo "PROMETHEUS_TLS_ENABLED=true"
echo "PROMETHEUS_TLS_CERT_FILE=$CERT_DIR/server.crt"
echo "PROMETHEUS_TLS_KEY_FILE=$CERT_DIR/server.key"
echo "# Uncomment to require a verified mTLS client certificate:"
echo "# PROMETHEUS_TLS_CA_FILE=$CERT_DIR/ca.crt"
echo "PROMETHEUS_TLS_MIN_VERSION=1.2"
echo ""
echo "Test HTTPS connection:"
echo "----------------------------------------------"
echo "curl --cacert $CERT_DIR/ca.crt https://localhost:1974/metrics"
echo ""
echo "Test with client certificate (mTLS):"
echo "----------------------------------------------"
echo "curl --cacert $CERT_DIR/ca.crt --cert $CERT_DIR/client.crt --key $CERT_DIR/client.key https://localhost:1974/metrics"
echo ""
echo "Prometheus scrape configuration:"
echo "----------------------------------------------"
cat << EOF
scrape_configs:
  - job_name: 'resman-https'
    scheme: https
    tls_config:
      ca_file: $CERT_DIR/ca.crt
      cert_file: $CERT_DIR/client.crt    # Optional for mTLS
      key_file: $CERT_DIR/client.key     # Optional for mTLS
      insecure_skip_verify: false
    # Uncomment when PROMETHEUS_AUTH_TYPE=basic is configured on ResMan.
    # basic_auth:
    #   username: prometheus
    #   password_file: /etc/prometheus/resman_password
    static_configs:
      - targets: ['resman:1974']
EOF
echo ""
echo "IMPORTANT: Backup your CA private key (ca.key) in a secure location!"
echo ""
