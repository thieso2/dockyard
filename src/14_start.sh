cmd_start() {
    require_root

    export PATH="${BIN_DIR}:${PATH}"

    mkdir -p "$LOG_DIR" "$RUN_DIR" "${RUN_DIR}/containerd" "$DOCKER_DATA/containerd"

    # Clean up stale sockets/pids from previous runs
    rm -f "$CONTAINERD_SOCKET" "$DOCKER_SOCKET"
    rm -f "${SYSBOX_RUN_DIR}/sysmgr.sock" "${SYSBOX_RUN_DIR}/sysfs.sock" "${SYSBOX_RUN_DIR}/sysfs-seccomp.sock"
    for pidfile in "${RUN_DIR}/containerd.pid" "${RUN_DIR}/dockerd.pid"; do
        if [ -f "$pidfile" ]; then
            local pid
            pid=$(cat "$pidfile")
            kill "$pid" 2>/dev/null && sleep 1 || true
            rm -f "$pidfile"
        fi
    done

    # Cleanup helper: kill previously started daemons on failure
    STARTED_PIDS=()
    cleanup() {
        echo "Startup failed — cleaning up..."
        for pid in "${STARTED_PIDS[@]}"; do
            kill "$pid" 2>/dev/null || true
        done
        if ip link show "$BRIDGE" &>/dev/null; then
            ip link set "$BRIDGE" down 2>/dev/null || true
            ip link delete "$BRIDGE" 2>/dev/null || true
        fi
        exit 1
    }

    # --- 1. Start per-instance sysbox daemons ---
    mkdir -p "$SYSBOX_RUN_DIR" "$SYSBOX_DATA_DIR"

    echo "Starting sysbox-mgr..."
    "${BIN_DIR}/sysbox-mgr" --run-dir "${SYSBOX_RUN_DIR}" --data-root "${SYSBOX_DATA_DIR}" \
        &>"${LOG_DIR}/sysbox-mgr.log" &
    SYSBOX_MGR_PID=$!
    echo "$SYSBOX_MGR_PID" > "${SYSBOX_RUN_DIR}/sysbox-mgr.pid"
    STARTED_PIDS+=("$SYSBOX_MGR_PID")
    wait_for_file "${SYSBOX_RUN_DIR}/sysmgr.sock" "sysbox-mgr" 60 || cleanup
    kill -0 "$SYSBOX_MGR_PID" 2>/dev/null || { echo "sysbox-mgr exited unexpectedly" >&2; cleanup; }
    echo "  sysbox-mgr ready (pid ${SYSBOX_MGR_PID})"

    echo "Starting sysbox-fs..."
    "${BIN_DIR}/sysbox-fs" --run-dir "${SYSBOX_RUN_DIR}" --mountpoint "${SYSBOX_DATA_DIR}" \
        &>"${LOG_DIR}/sysbox-fs.log" &
    SYSBOX_FS_PID=$!
    echo "$SYSBOX_FS_PID" > "${SYSBOX_RUN_DIR}/sysbox-fs.pid"
    STARTED_PIDS+=("$SYSBOX_FS_PID")
    wait_for_file "${SYSBOX_RUN_DIR}/sysfs.sock" "sysbox-fs" 60 || cleanup
    kill -0 "$SYSBOX_FS_PID" 2>/dev/null || { echo "sysbox-fs exited unexpectedly" >&2; cleanup; }
    echo "  sysbox-fs ready (pid ${SYSBOX_FS_PID})"

    # --- 2. Create bridge ---
    if ! ip link show "$BRIDGE" &>/dev/null; then
        echo "Creating bridge ${BRIDGE}..."
        ip link add "$BRIDGE" type bridge
        ip addr add "$DOCKYARD_BRIDGE_CIDR" dev "$BRIDGE"
        ip link set "$BRIDGE" up
    else
        echo "Bridge ${BRIDGE} already exists"
    fi

    # Enable IP forwarding and bridge netfilter (needed for isolation.d iptables on bridge traffic)
    sysctl -w net.ipv4.ip_forward=1 >/dev/null
    modprobe br_netfilter 2>/dev/null || true
    sysctl -w net.bridge.bridge-nf-call-iptables=1 >/dev/null 2>&1 || true

    # Bridge rules (idempotent)
    iptables -C FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT
    iptables -C FORWARD -i "$BRIDGE" ! -o "$BRIDGE" -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD -i "$BRIDGE" ! -o "$BRIDGE" -j ACCEPT
    iptables -C FORWARD -o "$BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD -o "$BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    iptables -t nat -C POSTROUTING -s "$DOCKYARD_FIXED_CIDR" ! -o "$BRIDGE" -j MASQUERADE 2>/dev/null ||
        iptables -t nat -I POSTROUTING -s "$DOCKYARD_FIXED_CIDR" ! -o "$BRIDGE" -j MASQUERADE

    # Pool rules
    iptables -C FORWARD -s "$DOCKYARD_POOL_BASE" -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD -s "$DOCKYARD_POOL_BASE" -j ACCEPT
    iptables -C FORWARD -d "$DOCKYARD_POOL_BASE" -j ACCEPT 2>/dev/null ||
        iptables -I FORWARD -d "$DOCKYARD_POOL_BASE" -j ACCEPT
    iptables -t nat -C POSTROUTING -s "$DOCKYARD_POOL_BASE" -j MASQUERADE 2>/dev/null ||
        iptables -t nat -I POSTROUTING -s "$DOCKYARD_POOL_BASE" -j MASQUERADE

    # --- 3. Start containerd ---
    echo "Starting containerd..."
    "${BIN_DIR}/containerd" \
        --root "$DOCKER_DATA/containerd" \
        --state "${RUN_DIR}/containerd" \
        --address "$CONTAINERD_SOCKET" \
        &>"${LOG_DIR}/containerd.log" &
    CONTAINERD_PID=$!
    echo "$CONTAINERD_PID" > "${RUN_DIR}/containerd.pid"
    STARTED_PIDS+=("$CONTAINERD_PID")

    wait_for_file "$CONTAINERD_SOCKET" "containerd" || cleanup
    echo "  containerd ready (pid ${CONTAINERD_PID})"

    # --- 4. Start dockerd ---
    echo "Starting dockerd..."
    "${BIN_DIR}/dockerd" \
        --config-file "${ETC_DIR}/daemon.json" \
        --containerd "$CONTAINERD_SOCKET" \
        --data-root "$DOCKER_DATA" \
        --exec-root "$RUN_DIR" \
        --pidfile "${RUN_DIR}/dockerd.pid" \
        --bridge "$BRIDGE" \
        --fixed-cidr "$DOCKYARD_FIXED_CIDR" \
        --default-address-pool "base=${DOCKYARD_POOL_BASE},size=${DOCKYARD_POOL_SIZE}" \
        --host "unix://${DOCKER_SOCKET}" \
        --iptables=false \
        --group "${INSTANCE_GROUP}" \
        &>"${LOG_DIR}/dockerd.log" &
    DOCKERD_PID=$!
    STARTED_PIDS+=("$DOCKERD_PID")

    wait_for_file "$DOCKER_SOCKET" "dockerd" 30 || cleanup
    echo "  dockerd ready (pid ${DOCKERD_PID})"

    # Apply isolation rules from ${ETC_DIR}/isolation.d/ if any .rules files exist.
    # Each .rules file lists IPs to ACCEPT; all other intra-bridge traffic is DROPped.
    local isolation_dir="${ETC_DIR}/isolation.d"
    local iso_chain="DOCKYARD-ISO-${DOCKYARD_DOCKER_PREFIX%_}"
    if ls "${isolation_dir}"/*.rules >/dev/null 2>&1; then
        # Build the chain once (not per-bridge)
        iptables -L "$iso_chain" >/dev/null 2>&1 || iptables -N "$iso_chain"
        iptables -F "$iso_chain"
        iptables -A "$iso_chain" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
        for f in "${isolation_dir}"/*.rules; do
            [ -f "$f" ] || continue
            while IFS= read -r line; do
                line="${line%%#*}"          # strip comments
                line="${line// /}"          # strip spaces
                [ -n "$line" ] || continue
                iptables -A "$iso_chain" -s "$line" -j ACCEPT
                iptables -A "$iso_chain" -d "$line" -j ACCEPT
            done < "$f"
        done
        iptables -A "$iso_chain" -j DROP

        # Add per-bridge jump rules for this instance's user-defined networks
        for net_id in $("${BIN_DIR}/docker-cli" -H "unix://${DOCKER_SOCKET}" network ls \
                --filter driver=bridge --format '{{.ID}}' 2>/dev/null); do
            local br="br-${net_id:0:12}"
            ip link show "$br" &>/dev/null || continue
            iptables -C FORWARD -i "$br" -o "$br" -j "$iso_chain" 2>/dev/null ||
                iptables -I FORWARD -i "$br" -o "$br" -j "$iso_chain"
            echo "  isolation rules applied on ${br}"
        done
    fi

    echo "=== All daemons started ==="
    echo "Run: DOCKER_HOST=unix://${DOCKER_SOCKET} docker ps"
}
