#!/bin/sh
set -eu

if [ "$(printf '%s' "${OBOT_SERVER_ENCRYPTION_PROVIDER:-}" | tr '[:upper:]' '[:lower:]')" = "custom" ]; then
  if [ -z "${OBOT_SERVER_ENCRYPTION_KEY:-}" ]; then
    echo "OBOT_SERVER_ENCRYPTION_KEY is required for custom encryption" >&2
    exit 1
  fi
  cat > /tmp/obot-encryption.yaml <<EOF
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
      - credentials.obot.obot.ai
      - users.obot.obot.ai
      - identities.obot.obot.ai
      - mcpoauthtokens.obot.obot.ai
      - mcpauditlogs.obot.obot.ai
      - llmauditlogs.obot.obot.ai
      - mcpoauthpendingstates.obot.obot.ai
      - policyviolations.obot.obot.ai
      - properties.obot.obot.ai
    providers:
      - aesgcm:
          keys:
            - name: key0
              secret: "${OBOT_SERVER_ENCRYPTION_KEY}"
      - identity: {}
EOF
  chmod 600 /tmp/obot-encryption.yaml
  export OBOT_SERVER_ENCRYPTION_CONFIG_FILE=/tmp/obot-encryption.yaml
fi

exec /bin/run.sh
