cmd_enable() {
    require_root

    local SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

    if [ -f "$SERVICE_FILE" ]; then
        echo "Error: ${SERVICE_FILE} already exists." >&2
        exit 1
    fi

    echo "Installing ${SERVICE_NAME}.service (with per-instance sysbox)..."

    # Write the stack script (bakes all paths in at install time; no env file at runtime)
    cat > "${BIN_DIR}/dockyard-stack" <<STACKEOF
#!/bin/bash
set -euo pipefail

MGR_PID=""
FS_PID=""
CTR_PID=""
DOCKERD_PID=""

wait_for_socket() {
    local sock="\$1" pid="\$2" name="\$3" i=0
    while [ ! -S "\$sock" ]; do
        sleep 1
        i=\$((i+1))
        if ! kill -0 "\$pid" 2>/dev/null; then
            echo "dockyard-stack: \$name exited unexpectedly" >&2
            return 1
        fi
        if [ "\$i" -ge 60 ]; then
            echo "dockyard-stack: \$name did not start within 60s" >&2
            return 1
        fi
    done
}

cleanup() {
    local code=\${1:-0}
    for pid in "\$DOCKERD_PID" "\$CTR_PID" "\$FS_PID" "\$MGR_PID"; do
        [ -z "\$pid" ] && continue
        kill "\$pid" 2>/dev/null || true
        wait "\$pid" 2>/dev/null || true
    done
    exit "\$code"
}

trap 'cleanup 0' TERM INT

# --- Start sysbox-mgr ---
${BIN_DIR}/sysbox-mgr --run-dir ${SYSBOX_RUN_DIR} --data-root ${SYSBOX_DATA_DIR} \
    >>${LOG_DIR}/sysbox-mgr.log 2>&1 &
MGR_PID=\$!
echo "\$MGR_PID" > ${SYSBOX_RUN_DIR}/sysbox-mgr.pid
wait_for_socket ${SYSBOX_RUN_DIR}/sysmgr.sock "\$MGR_PID" sysbox-mgr || cleanup 1

# --- Start sysbox-fs ---
${BIN_DIR}/sysbox-fs --run-dir ${SYSBOX_RUN_DIR} --mountpoint ${SYSBOX_DATA_DIR} \
    >>${LOG_DIR}/sysbox-fs.log 2>&1 &
FS_PID=\$!
echo "\$FS_PID" > ${SYSBOX_RUN_DIR}/sysbox-fs.pid
wait_for_socket ${SYSBOX_RUN_DIR}/sysfs.sock "\$FS_PID" sysbox-fs || cleanup 1

# --- Start containerd ---
${BIN_DIR}/containerd \
    --root ${DOCKER_DATA}/containerd \
    --state ${RUN_DIR}/containerd \
    --address ${CONTAINERD_SOCKET} \
    >>${LOG_DIR}/containerd.log 2>&1 &
CTR_PID=\$!
echo "\$CTR_PID" > ${RUN_DIR}/containerd.pid
wait_for_socket ${CONTAINERD_SOCKET} "\$CTR_PID" containerd || cleanup 1

# --- Start dockerd ---
${BIN_DIR}/dockerd \
    --config-file ${ETC_DIR}/daemon.json \
    --containerd ${CONTAINERD_SOCKET} \
    --data-root ${DOCKER_DATA} \
    --exec-root ${RUN_DIR} \
    --pidfile ${RUN_DIR}/dockerd.pid \
    --bridge ${BRIDGE} \
    --fixed-cidr ${DOCKYARD_FIXED_CIDR} \
    --default-address-pool base=${DOCKYARD_POOL_BASE},size=${DOCKYARD_POOL_SIZE} \
    --host unix://${DOCKER_SOCKET} \
    --iptables=false \
    --group ${INSTANCE_GROUP} \
    >>${LOG_DIR}/dockerd.log 2>&1 &
DOCKERD_PID=\$!
wait_for_socket ${DOCKER_SOCKET} "\$DOCKERD_PID" dockerd || cleanup 1

# Signal systemd that all daemon sockets are up.
# || true: sd_notify returns non-zero when not running under systemd; don't abort.
systemd-notify --ready 2>/dev/null || true

# Monitor: if any daemon dies, trigger a restart
while true; do
    sleep 2 &
    wait \$! 2>/dev/null || true
    for check in "sysbox-mgr:\$MGR_PID" "sysbox-fs:\$FS_PID" "containerd:\$CTR_PID" "dockerd:\$DOCKERD_PID"; do
        _name="\${check%%:*}"
        _pid="\${check##*:}"
        if [ -n "\$_pid" ] && ! kill -0 "\$_pid" 2>/dev/null; then
            echo "dockyard-stack: \$_name (pid \$_pid) died unexpectedly" >&2
            cleanup 1
        fi
    done
done
STACKEOF
    chmod 755 "${BIN_DIR}/dockyard-stack"
    echo "  installed ${BIN_DIR}/dockyard-stack"

    local ISO_CHAIN="DOCKYARD-ISO-${DOCKYARD_DOCKER_PREFIX%_}"

    cat > "$SERVICE_FILE" <<SERVICEEOF
[Unit]
Description=Dockyard Docker (${SERVICE_NAME})
After=network-online.target nss-lookup.target firewalld.service time-set.target
Before=docker.service
Wants=network-online.target
RequiresMountsFor=${DOCKYARD_ROOT}
StartLimitBurst=3
StartLimitIntervalSec=60

[Service]
Type=notify
NotifyAccess=all
Environment=PATH=${BIN_DIR}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Create runtime and sysbox directories
ExecStartPre=/bin/mkdir -p ${LOG_DIR} ${RUN_DIR}/containerd ${SYSBOX_RUN_DIR} ${DOCKER_DATA}/containerd ${SYSBOX_DATA_DIR}

# Clean stale sockets (including sysbox — stale socket file fools the wait loop)
ExecStartPre=-/bin/rm -f ${CONTAINERD_SOCKET} ${DOCKER_SOCKET}
ExecStartPre=-/bin/rm -f ${SYSBOX_RUN_DIR}/sysmgr.sock ${SYSBOX_RUN_DIR}/sysfs.sock ${SYSBOX_RUN_DIR}/sysfs-seccomp.sock

# Enable IP forwarding and bridge netfilter (needed for isolation.d iptables on bridge traffic)
ExecStartPre=/bin/bash -c 'sysctl -w net.ipv4.ip_forward=1 >/dev/null; modprobe br_netfilter 2>/dev/null; sysctl -w net.bridge.bridge-nf-call-iptables=1 >/dev/null 2>&1 || true'

# Create bridge
ExecStartPre=/bin/bash -c 'if ! ip link show ${BRIDGE} &>/dev/null; then ip link add ${BRIDGE} type bridge && ip addr add ${DOCKYARD_BRIDGE_CIDR} dev ${BRIDGE} && ip link set ${BRIDGE} up; fi'

# Add iptables rules for container networking (bridge) — idempotent: -C check before -I
ExecStartPre=/bin/bash -c 'iptables -C FORWARD -i ${BRIDGE} -o ${BRIDGE} -j ACCEPT 2>/dev/null || iptables -I FORWARD -i ${BRIDGE} -o ${BRIDGE} -j ACCEPT; iptables -C FORWARD -i ${BRIDGE} ! -o ${BRIDGE} -j ACCEPT 2>/dev/null || iptables -I FORWARD -i ${BRIDGE} ! -o ${BRIDGE} -j ACCEPT; iptables -C FORWARD -o ${BRIDGE} -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -I FORWARD -o ${BRIDGE} -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT; iptables -t nat -C POSTROUTING -s ${DOCKYARD_FIXED_CIDR} ! -o ${BRIDGE} -j MASQUERADE 2>/dev/null || iptables -t nat -I POSTROUTING -s ${DOCKYARD_FIXED_CIDR} ! -o ${BRIDGE} -j MASQUERADE'

# Add iptables rules for user-defined networks (from default-address-pool) — idempotent
ExecStartPre=/bin/bash -c 'iptables -C FORWARD -s ${DOCKYARD_POOL_BASE} -j ACCEPT 2>/dev/null || iptables -I FORWARD -s ${DOCKYARD_POOL_BASE} -j ACCEPT; iptables -C FORWARD -d ${DOCKYARD_POOL_BASE} -j ACCEPT 2>/dev/null || iptables -I FORWARD -d ${DOCKYARD_POOL_BASE} -j ACCEPT; iptables -t nat -C POSTROUTING -s ${DOCKYARD_POOL_BASE} -j MASQUERADE 2>/dev/null || iptables -t nat -I POSTROUTING -s ${DOCKYARD_POOL_BASE} -j MASQUERADE'

# Stack script starts sysbox-mgr, sysbox-fs, containerd, dockerd; sends READY once sockets
# are up; then monitors. ExecStartPost gates systemctl-start on full API readiness so callers
# of "systemctl start" don't proceed until docker accepts connections.
ExecStart=${BIN_DIR}/dockyard-stack

# Wait until dockerd accepts API connections before systemctl start returns.
ExecStartPost=/bin/bash -c 'i=0; while ! ${BIN_DIR}/docker-cli -H unix://${DOCKER_SOCKET} info >/dev/null 2>&1; do i=\$((i+1)); [ \$i -ge 360 ] && exit 1; sleep 0.5; done'

# Apply isolation rules from ${ETC_DIR}/isolation.d/ if any .rules files exist.
# Each .rules file lists IPs to ACCEPT; all other intra-bridge traffic is DROPped.
# Chain ${ISO_CHAIN} is per-instance; built once, then jump rules added per user-defined bridge.
ExecStartPost=-/bin/bash -c 'dir=${ETC_DIR}/isolation.d; ls "\$dir"/*.rules >/dev/null 2>&1 || exit 0; iptables -L ${ISO_CHAIN} >/dev/null 2>&1 || iptables -N ${ISO_CHAIN}; iptables -F ${ISO_CHAIN}; iptables -A ${ISO_CHAIN} -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT; for f in "\$dir"/*.rules; do [ -f "\$f" ] || continue; sed "s/#.*//;s/ //g;/^$/d" "\$f" | while IFS= read -r line; do iptables -A ${ISO_CHAIN} -s "\$line" -j ACCEPT; iptables -A ${ISO_CHAIN} -d "\$line" -j ACCEPT; done; done; iptables -A ${ISO_CHAIN} -j DROP; for net_id in \$(${BIN_DIR}/docker-cli -H unix://${DOCKER_SOCKET} network ls --filter driver=bridge --format "{{.ID}}" 2>/dev/null); do br=br-\$(echo \$net_id | cut -c1-12); ip link show "\$br" &>/dev/null || continue; iptables -C FORWARD -i "\$br" -o "\$br" -j ${ISO_CHAIN} 2>/dev/null || iptables -I FORWARD -i "\$br" -o "\$br" -j ${ISO_CHAIN}; done'

# Clean up docker/containerd sockets
ExecStopPost=-/bin/rm -f ${DOCKER_SOCKET} ${CONTAINERD_SOCKET}

# Remove iptables rules (bridge)
ExecStopPost=-/bin/bash -c 'iptables -D FORWARD -i ${BRIDGE} -o ${BRIDGE} -j ACCEPT 2>/dev/null; iptables -D FORWARD -i ${BRIDGE} ! -o ${BRIDGE} -j ACCEPT 2>/dev/null; iptables -D FORWARD -o ${BRIDGE} -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; iptables -t nat -D POSTROUTING -s ${DOCKYARD_FIXED_CIDR} ! -o ${BRIDGE} -j MASQUERADE 2>/dev/null'

# Remove iptables rules (user-defined networks)
ExecStopPost=-/bin/bash -c 'iptables -D FORWARD -s ${DOCKYARD_POOL_BASE} -j ACCEPT 2>/dev/null; iptables -D FORWARD -d ${DOCKYARD_POOL_BASE} -j ACCEPT 2>/dev/null; iptables -t nat -D POSTROUTING -s ${DOCKYARD_POOL_BASE} -j MASQUERADE 2>/dev/null'

# Remove per-instance isolation chain and its jump rules
ExecStopPost=-/bin/bash -c 'for br in \$(ip -o link show type bridge 2>/dev/null | grep -oP "br-[0-9a-f]+"); do iptables -D FORWARD -i "\$br" -o "\$br" -j ${ISO_CHAIN} 2>/dev/null; done; iptables -F ${ISO_CHAIN} 2>/dev/null; iptables -X ${ISO_CHAIN} 2>/dev/null'

# Remove bridge
ExecStopPost=-/bin/bash -c 'if ip link show ${BRIDGE} &>/dev/null; then ip link set ${BRIDGE} down 2>/dev/null; ip link delete ${BRIDGE} 2>/dev/null; fi'

# Clean up sysbox run dir
ExecStopPost=-/bin/rm -rf ${SYSBOX_RUN_DIR}

TimeoutStartSec=180
TimeoutStopSec=30
Restart=on-failure
RestartSec=5

LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
OOMScoreAdjust=-500

[Install]
WantedBy=multi-user.target
SERVICEEOF
    chmod 644 "$SERVICE_FILE"
    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}.service"
    echo "  enabled ${SERVICE_NAME}.service (will start on boot)"
    echo ""
    echo "  sudo systemctl start ${SERVICE_NAME}    # start"
    echo "  sudo systemctl status ${SERVICE_NAME}   # check status"
    echo "  sudo journalctl -u ${SERVICE_NAME} -f   # follow logs"
}
