#!/bin/sh
set -eu

: "${QUICKWORKS_AGENT_CONTROL_URL:?missing control URL}"
: "${QUICKWORKS_AGENT_ID:?missing agent ID}"
: "${QUICKWORKS_AGENT_ENROLLMENT_TOKEN:?missing enrollment token}"

if ! getent group workspace >/dev/null 2>&1; then
  groupadd --system workspace
fi
if id -u workspace >/dev/null 2>&1; then
  usermod --gid workspace --home /home/workspace --shell /bin/bash workspace
else
  useradd --system --gid workspace --home-dir /home/workspace \
    --create-home --shell /bin/bash workspace
fi

install -d -m 0700 -o root -g root /var/lib/quickworks-agent
install -d -m 0700 -o root -g root /var/lib/quickworks-agent/workbench/versions
chown root:workspace /var/lib/quickworks-agent
chmod 0710 /var/lib/quickworks-agent
chmod 0755 /var/lib/quickworks-agent/workbench/versions
install -d -m 0755 -o root -g root /etc/quickworks
install -d -m 0750 -o workspace -g workspace /home/workspace
install -d -m 0750 -o workspace -g workspace /workspace
runuser -u workspace -- git config --global credential.helper store
runuser -u workspace -- git config --global credential.useHttpPath true
install -d -m 0750 -o root -g root /etc/sudoers.d
printf '%s\n' 'workspace ALL=(ALL) NOPASSWD: ALL' >/etc/sudoers.d/workspace
chown root:root /etc/sudoers.d/workspace
chmod 0440 /etc/sudoers.d/workspace
visudo -cf /etc/sudoers.d/workspace

release_dir="$(mktemp -d /run/quickworks-release.XXXXXX)"
trap 'rm -rf "$release_dir"' EXIT

curl --fail --silent --show-error --location \
  "$QUICKWORKS_AGENT_CONTROL_URL/assets/quickworks-linux-amd64.tar.gz" \
  --output "$release_dir/bundle.tar.gz"
curl --fail --silent --show-error --location \
  "$QUICKWORKS_AGENT_CONTROL_URL/assets/quickworks-linux-amd64.tar.gz.sha256" \
  --output "$release_dir/bundle.tar.gz.sha256"

expected_sha256="$(awk '{ print $1 }' "$release_dir/bundle.tar.gz.sha256")"
printf '%s  %s\n' "$expected_sha256" "$release_dir/bundle.tar.gz" | sha256sum --check --status

tar -xzf "$release_dir/bundle.tar.gz" -C "$release_dir" quickworks
install -m 0755 "$release_dir/quickworks" /usr/local/bin/quickworks

cat >/etc/quickworks/agent.env <<EOF
QUICKWORKS_AGENT_ID=$QUICKWORKS_AGENT_ID
QUICKWORKS_AGENT_CONTROL_URL=$QUICKWORKS_AGENT_CONTROL_URL
QUICKWORKS_AGENT_ENROLLMENT_TOKEN=$QUICKWORKS_AGENT_ENROLLMENT_TOKEN
QUICKWORKS_AGENT_STATE_DIR=/var/lib/quickworks-agent
QUICKWORKS_AGENT_UPSTREAM=http://127.0.0.1:3000
QUICKWORKS_AGENT_HEALTH_URL=http://127.0.0.1:3000/healthz
QUICKWORKS_WORKBENCH_USER=workspace
QUICKWORKS_WORKBENCH_GROUP=workspace
QUICKWORKS_WORKBENCH_BASE_PATH=${QUICKWORKS_BASE_PATH:-/}
QUICKWORKS_WORKBENCH_PORT=3000
QUICKWORKS_WORKSPACE_DIR=/workspace
REPOSITORY_URL=${QUICKWORKS_REPOSITORY_URL:-}
EOF
chmod 0600 /etc/quickworks/agent.env

cat >/etc/systemd/system/quickworks-agent.service <<'EOF'
[Unit]
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/quickworks/agent.env
ExecStart=/usr/local/bin/quickworks agent
Restart=always
RestartSec=2
UMask=0077

[Install]
WantedBy=multi-user.target
EOF

cat >/etc/systemd/system/quickworks-update.service <<'EOF'
[Unit]
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/quickworks/agent.env
ExecStart=/bin/sh -c 'curl --fail --silent --show-error --location "$QUICKWORKS_AGENT_CONTROL_URL/assets/workspace-bootstrap.sh" --output /usr/local/bin/quickworks-bootstrap && chmod 0700 /usr/local/bin/quickworks-bootstrap && /usr/local/bin/quickworks-bootstrap'
EOF

cat >/etc/systemd/system/quickworks-update.timer <<'EOF'
[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
RandomizedDelaySec=2min
Persistent=true

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl disable --now quickworks-workbench.service 2>/dev/null || true
rm -f /etc/systemd/system/quickworks-workbench.service
systemctl daemon-reload
systemctl enable quickworks-agent.service
systemctl enable --now quickworks-update.timer
systemctl try-restart quickworks-agent.service || systemctl start quickworks-agent.service
