#!/usr/bin/env bash
# Creates the key pair Sealed Secrets uses, once, outside the cluster.
#
#   key.yaml      the private key as a Secret manifest. Keep it secret and back
#                 it up. Ansible installs it on every new node, so files sealed
#                 earlier open there. Never commit it.
#   pub-cert.pem  the public certificate. It can only seal, so it is safe to
#                 commit, and kubeseal can use it without a cluster.
#
# Usage: sealed-secrets/generate-key.sh [directory]
set -euo pipefail

dir="${1:-$HOME/.config/platformator/sealed-secrets}"

if [ -e "$dir/key.yaml" ]; then
  echo "$dir/key.yaml already exists, not replacing a key that sealed files may depend on" >&2
  exit 1
fi

umask 077
mkdir -p "$dir"

openssl req -x509 -days 3650 -nodes -newkey rsa:4096 \
  -keyout "$dir/tls.key" -out "$dir/pub-cert.pem" \
  -subj "/CN=sealed-secret/O=sealed-secret" 2>/dev/null

cat > "$dir/key.yaml" <<MANIFEST
apiVersion: v1
kind: Secret
metadata:
  name: sealed-secrets-key-platformator
  namespace: kube-system
  labels:
    sealedsecrets.bitnami.com/sealed-secrets-key: active
type: kubernetes.io/tls
data:
  tls.crt: $(base64 < "$dir/pub-cert.pem" | tr -d '\n')
  tls.key: $(base64 < "$dir/tls.key" | tr -d '\n')
MANIFEST

rm -f "$dir/tls.key"

echo "Private key (back it up, never commit): $dir/key.yaml"
echo "Public certificate (safe to commit):    $dir/pub-cert.pem"
