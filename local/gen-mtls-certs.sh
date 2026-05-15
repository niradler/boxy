#!/usr/bin/env bash
# Generate mTLS certificates for a boxy production deployment.
#
# Outputs:
#   ca.crt             — CA certificate (distribute to all components)
#   ca.key             — CA private key (keep secret; used only during issuance)
#   server.crt         — controller server certificate (CN=boxy-controller)
#   server.key         — controller server private key
#   client.crt         — router/operator client certificate (CN=boxy-router)
#   client.key         — router/operator client private key
#
# Usage:
#   bash local/gen-mtls-certs.sh [output-dir] [validity-days]
#
# Defaults:
#   output-dir    = ./certs
#   validity-days = 1095  (3 years)
#
# To use with helm:
#   kubectl -n boxy create secret generic boxy-mtls-ca \
#     --from-file=ca.crt=certs/ca.crt
#   helm upgrade boxy ./deploy/helm/boxy \
#     --set-file mtls.caCert=certs/ca.crt \
#     --set-file mtls.serverCert=certs/server.crt \
#     --set-file mtls.serverKey=certs/server.key \
#     --set-file mtls.clientCert=certs/client.crt \
#     --set-file mtls.clientKey=certs/client.key
set -euo pipefail

OUT_DIR="${1:-./certs}"
VALIDITY_DAYS="${2:-1095}"

CA_CN="${BOXY_CA_CN:-boxy-ca}"
SERVER_CN="${BOXY_SERVER_CN:-boxy-controller}"
CLIENT_CN="${BOXY_CLIENT_CN:-boxy-router}"

mkdir -p "${OUT_DIR}"

echo ">>> Generating CA (CN=${CA_CN}, valid ${VALIDITY_DAYS} days)"
openssl genrsa -out "${OUT_DIR}/ca.key" 4096 2>/dev/null
openssl req -new -x509 -days "${VALIDITY_DAYS}" \
    -key "${OUT_DIR}/ca.key" \
    -out "${OUT_DIR}/ca.crt" \
    -subj "/CN=${CA_CN}/O=boxy"

echo ">>> Generating server cert (CN=${SERVER_CN})"
openssl genrsa -out "${OUT_DIR}/server.key" 2048 2>/dev/null
openssl req -new \
    -key "${OUT_DIR}/server.key" \
    -out "${OUT_DIR}/server.csr" \
    -subj "/CN=${SERVER_CN}/O=boxy"
openssl x509 -req -days "${VALIDITY_DAYS}" \
    -in  "${OUT_DIR}/server.csr" \
    -CA  "${OUT_DIR}/ca.crt" \
    -CAkey "${OUT_DIR}/ca.key" \
    -CAcreateserial \
    -extfile <(printf "subjectAltName=DNS:%s,DNS:*.boxy-ctrl-headless.boxy.svc" "${SERVER_CN}") \
    -out "${OUT_DIR}/server.crt" 2>/dev/null
rm -f "${OUT_DIR}/server.csr"

echo ">>> Generating client cert (CN=${CLIENT_CN})"
openssl genrsa -out "${OUT_DIR}/client.key" 2048 2>/dev/null
openssl req -new \
    -key "${OUT_DIR}/client.key" \
    -out "${OUT_DIR}/client.csr" \
    -subj "/CN=${CLIENT_CN}/O=boxy"
openssl x509 -req -days "${VALIDITY_DAYS}" \
    -in  "${OUT_DIR}/client.csr" \
    -CA  "${OUT_DIR}/ca.crt" \
    -CAkey "${OUT_DIR}/ca.key" \
    -CAcreateserial \
    -extfile <(printf "extendedKeyUsage=clientAuth") \
    -out "${OUT_DIR}/client.crt" 2>/dev/null
rm -f "${OUT_DIR}/client.csr"

echo ""
echo "Certificates written to ${OUT_DIR}/"
echo ""
echo "  CA cert:         ${OUT_DIR}/ca.crt   (expires $(openssl x509 -noout -enddate -in "${OUT_DIR}/ca.crt" | cut -d= -f2))"
echo "  Server cert:     ${OUT_DIR}/server.crt"
echo "  Client cert:     ${OUT_DIR}/client.crt"
echo ""
echo "Next step — create the Helm mTLS secret:"
echo "  kubectl -n boxy create secret generic boxy-mtls \\"
echo "    --from-file=ca.crt=${OUT_DIR}/ca.crt \\"
echo "    --from-file=server.crt=${OUT_DIR}/server.crt \\"
echo "    --from-file=server.key=${OUT_DIR}/server.key \\"
echo "    --from-file=client.crt=${OUT_DIR}/client.crt \\"
echo "    --from-file=client.key=${OUT_DIR}/client.key"
