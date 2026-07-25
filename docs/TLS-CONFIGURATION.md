# TLS/HTTPS Configuration Guide for CPU Manager Go

This guide explains how to enable HTTPS/TLS encryption for the CPU Manager Go Prometheus metrics endpoint.

## Table of Contents

1. [Overview](#overview)
2. [Prerequisites](#prerequisites)
3. [Generate Certificates](#generate-certificates)
4. [Configuration](#configuration)
5. [Testing](#testing)
6. [Prometheus Configuration](#prometheus-configuration)
7. [Troubleshooting](#troubleshooting)

---

## Overview

CPU Manager Go supports TLS/HTTPS encryption for securing metrics endpoints in production environments. This provides:

- **Encryption in transit** - All metrics data encrypted with TLS 1.2+
- **Server authentication** - Clients can verify server identity
- **Client authentication (mTLS)** - Optional mutual TLS for client verification
- **Combined with authentication** - Works with Basic Auth and/or JWT

---

## Prerequisites

- OpenSSL installed (`openssl` command)
- Root or sudo access
- CPU Manager Go installed

---

## Generate Certificates

### Option 1: Automated Script (Recommended)

Use the provided script to generate all certificates:

```bash
cd /path/to/resman-go
sudo ./docs/generate-tls-certs.sh /etc/resman/tls
```

This creates:
- `ca.crt` / `ca.key` - Certificate Authority
- `server.crt` / `server.key` - Server certificate and key
- `client.crt` / `client.key` - Client certificate (for mTLS)

### Option 2: Manual Generation

#### 1. Generate CA

```bash
mkdir -p /etc/resman/tls
cd /etc/resman/tls

# Generate CA private key
openssl genrsa -out ca.key 4096

# Generate CA certificate
openssl req -x509 -new -nodes -sha256 -days 365 \
    -key ca.key \
    -out ca.crt \
    -subj "/C=IT/ST=Italy/L=Rome/O=CPU Manager/OU=Monitoring/CN=CPU Manager CA"
```

#### 2. Generate Server Certificate

```bash
# Generate server private key
openssl genrsa -out server.key 2048

# Generate server CSR
openssl req -new -sha256 \
    -key server.key \
    -out server.csr \
    -subj "/C=IT/ST=Italy/L=Rome/O=CPU Manager/OU=Monitoring/CN=resman.local"

# Create SAN extension file
cat > server_ext.cnf << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = resman.local
DNS.2 = localhost
IP.1 = 127.0.0.1
EOF

# Sign server certificate with CA
openssl x509 -req -sha256 -days 365 \
    -in server.csr \
    -CA ca.crt \
    -CAkey ca.key \
    -CAcreateserial \
    -out server.crt \
    -extfile server_ext.cnf

# Cleanup
rm -f server.csr server_ext.cnf
```

#### 3. Generate Client Certificate (Optional, for mTLS)

```bash
# Generate client private key
openssl genrsa -out client.key 2048

# Generate client CSR
openssl req -new -sha256 \
    -key client.key \
    -out client.csr \
    -subj "/C=IT/ST=Italy/L=Rome/O=CPU Manager/OU=Monitoring/CN=prometheus"

# Create client extension file
cat > client_ext.cnf << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
extendedKeyUsage = clientAuth
EOF

# Sign client certificate with CA
openssl x509 -req -sha256 -days 365 \
    -in client.csr \
    -CA ca.crt \
    -CAkey ca.key \
    -CAcreateserial \
    -out client.crt \
    -extfile client_ext.cnf

# Cleanup
rm -f client.csr client_ext.cnf
```

#### 4. Set Permissions

```bash
chmod 644 ca.crt server.crt client.crt
chmod 600 ca.key server.key client.key
chown -R root:root /etc/resman/tls
```

---

## Configuration

### Enable TLS in CPU Manager

Edit `/etc/resman.conf`:

```bash
# Enable TLS/HTTPS
PROMETHEUS_TLS_ENABLED=true

# Certificate files
PROMETHEUS_TLS_CERT_FILE=/etc/resman/tls/server.crt
PROMETHEUS_TLS_KEY_FILE=/etc/resman/tls/server.key

# Minimum TLS version (recommended: 1.2 or 1.3)
PROMETHEUS_TLS_MIN_VERSION=1.2
```

This enables one-way TLS: Prometheus verifies the resman server certificate.
To require mutual TLS, also configure the CA that signed the Prometheus client
certificate:

```bash
PROMETHEUS_TLS_CA_FILE=/etc/resman/tls/ca.crt
```

When this field is set, every HTTPS request, including `/health`, must present a
valid client certificate signed by that CA.

### Combine with Authentication (Recommended)

For maximum security, combine TLS with authentication:

```bash
# TLS Configuration
PROMETHEUS_TLS_ENABLED=true
PROMETHEUS_TLS_CERT_FILE=/etc/resman/tls/server.crt
PROMETHEUS_TLS_KEY_FILE=/etc/resman/tls/server.key

# Basic Authentication
PROMETHEUS_AUTH_TYPE=basic
PROMETHEUS_AUTH_USERNAME=prometheus
PROMETHEUS_AUTH_PASSWORD_FILE=/etc/resman/prometheus_password

# Or JWT Authentication
# PROMETHEUS_AUTH_TYPE=jwt
# PROMETHEUS_JWT_SECRET_FILE=/etc/resman/jwt_secret
```

### Restart CPU Manager

```bash
sudo systemctl restart resman
```

---

## Testing

### Test HTTPS Connection

```bash
# With CA certificate
curl --cacert /etc/resman/tls/ca.crt https://localhost:9101/metrics

# With Basic Auth
curl --cacert /etc/resman/tls/ca.crt \
     -u prometheus:password \
     https://localhost:9101/metrics

# With mTLS (client certificate)
curl --cacert /etc/resman/tls/ca.crt \
     --cert /etc/resman/tls/client.crt \
     --key /etc/resman/tls/client.key \
     https://localhost:9101/metrics
```

### Verify TLS Version

```bash
# Check TLS version in use
curl -v --cacert /etc/resman/tls/ca.crt https://localhost:9101/metrics 2>&1 | grep "TLS"
```

### Check Logs

```bash
# View CPU Manager logs
sudo journalctl -u resman -f

# Look for TLS-related messages
sudo journalctl -u resman | grep -i tls
```

---

## Prometheus Configuration

### Basic HTTPS Configuration

```yaml
# /etc/prometheus/prometheus.yml
scrape_configs:
  - job_name: 'resman-https'
    scheme: https
    
    tls_config:
      ca_file: /etc/prometheus/certs/resman-ca.crt
    
    basic_auth:
      username: prometheus
      password_file: /etc/prometheus/credentials/cpu_manager_password
    
    static_configs:
      - targets: ['resman.example.com:9101']
```

### mTLS Configuration (Mutual TLS)

```yaml
scrape_configs:
  - job_name: 'resman-mtls'
    scheme: https
    
    tls_config:
      ca_file: /etc/prometheus/certs/resman-ca.crt
      cert_file: /etc/prometheus/certs/resman-client.crt
      key_file: /etc/prometheus/certs/resman-client.key
      insecure_skip_verify: false
    
    static_configs:
      - targets: ['resman.example.com:9101']
```

### Copy Certificates to Prometheus

```bash
# Create Prometheus certificate directory
sudo mkdir -p /etc/prometheus/certs

# Copy CA certificate
sudo cp /etc/resman/tls/ca.crt /etc/prometheus/certs/resman-ca.crt

# Copy client certificate (for mTLS)
sudo cp /etc/resman/tls/client.crt /etc/prometheus/certs/resman-client.crt
sudo cp /etc/resman/tls/client.key /etc/prometheus/certs/resman-client.key

# Set permissions
sudo chmod 644 /etc/prometheus/certs/*.crt
sudo chmod 600 /etc/prometheus/certs/*.key
sudo chown prometheus:prometheus /etc/prometheus/certs/*

# Reload Prometheus
sudo systemctl reload prometheus
```

---

## Troubleshooting

### Certificate Verification Failed

**Error**: `x509: certificate signed by unknown authority`

**Solution**: Ensure CA certificate is correctly specified in Prometheus config:
```yaml
tls_config:
  ca_file: /etc/prometheus/certs/resman-ca.crt
```

### TLS Handshake Failed

**Error**: `tls: failed to verify certificate`

**Solutions**:
1. Check certificate validity: `openssl x509 -in server.crt -text -noout`
2. Verify certificate dates: `openssl x509 -in server.crt -dates -noout`
3. Check SAN matches hostname: `openssl x509 -in server.crt -text -noout | grep -A1 "Subject Alternative Name"`

### Connection Refused

**Error**: `connection refused`

**Solutions**:
1. Verify CPU Manager is running: `systemctl status resman`
2. Check if HTTPS is enabled: `grep PROMETHEUS_TLS_ENABLED /etc/resman.conf`
3. Verify port is listening: `netstat -tlnp | grep 9101`

### Certificate Expired

**Solution**: Regenerate certificates:
```bash
sudo ./docs/generate-tls-certs.sh /etc/resman/tls
sudo systemctl restart resman
```

---

## Security Best Practices

### Certificate Management

- **Validity period**: Use 1 year or less for certificates
- **Key size**: Minimum 2048 bits for RSA keys
- **TLS version**: Minimum TLS 1.2, prefer TLS 1.3
- **CA backup**: Store CA private key offline in secure location
- **Rotation**: Rotate certificates before expiration

### File Permissions

```bash
# Private keys: readable only by owner
chmod 600 *.key

# Certificates: readable by all
chmod 644 *.crt *.pem

# Ownership: root only
chown -R root:root /etc/resman/tls
```

### Network Security

- **Firewall**: Restrict access to port 9101
- **Internal network**: Use dedicated monitoring VLAN
- **No HTTP fallback**: Disable HTTP when HTTPS is enabled

### Combined Security

For production environments, use:

```bash
# TLS + Authentication
PROMETHEUS_TLS_ENABLED=true
PROMETHEUS_AUTH_TYPE=jwt  # or basic
PROMETHEUS_TLS_MIN_VERSION=1.2
```

---

## Certificate Renewal

### Automated Renewal with Cron

Create a renewal script:

```bash
#!/bin/bash
# /usr/local/bin/renew-resman-certs.sh

CERT_DIR=/etc/resman/tls
BACKUP_DIR=/var/backup/resman-certs

# Create backup
mkdir -p $BACKUP_DIR
cp -r $CERT_DIR $BACKUP_DIR/certs-$(date +%Y%m%d)

# Regenerate certificates
/path/to/resman-go/docs/generate-tls-certs.sh $CERT_DIR

# Restart CPU Manager
systemctl restart resman

echo "Certificates renewed at $(date)"
```

Add to crontab (run 30 days before expiration):
```bash
# Renew certificates 330 days after generation (365 - 35 days buffer)
0 0 1 11 * /usr/local/bin/renew-resman-certs.sh
```

---

## See Also

- [Multi-Instance Monitoring Guide](MULTI-INSTANCE-MONITORING.md)
- [Prometheus Queries](prometheus-queries.md)
- [Alerting Rules](alerting-rules.yml)
