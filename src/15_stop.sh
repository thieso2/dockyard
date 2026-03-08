cmd_stop() {
    require_root

    # Reverse startup order: dockerd -> containerd -> sysbox
    stop_daemon dockerd "${RUN_DIR}/dockerd.pid" 20
    stop_daemon containerd "${RUN_DIR}/containerd.pid" 10
    stop_daemon sysbox-fs "${SYSBOX_RUN_DIR}/sysbox-fs.pid" 10
    stop_daemon sysbox-mgr "${SYSBOX_RUN_DIR}/sysbox-mgr.pid" 10
    rm -rf "$SYSBOX_RUN_DIR"

    # Clean up sockets
    rm -f "$DOCKER_SOCKET" "$CONTAINERD_SOCKET"

    # Remove iptables rules (bridge)
    iptables -D FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT 2>/dev/null || true
    iptables -D FORWARD -i "$BRIDGE" ! -o "$BRIDGE" -j ACCEPT 2>/dev/null || true
    iptables -D FORWARD -o "$BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
    iptables -t nat -D POSTROUTING -s "$DOCKYARD_FIXED_CIDR" ! -o "$BRIDGE" -j MASQUERADE 2>/dev/null || true
    # Remove iptables rules (pool)
    iptables -D FORWARD -s "$DOCKYARD_POOL_BASE" -j ACCEPT 2>/dev/null || true
    iptables -D FORWARD -d "$DOCKYARD_POOL_BASE" -j ACCEPT 2>/dev/null || true
    iptables -t nat -D POSTROUTING -s "$DOCKYARD_POOL_BASE" -j MASQUERADE 2>/dev/null || true

    # Remove per-instance isolation chain and its jump rules
    local iso_chain="DOCKYARD-ISO-${DOCKYARD_DOCKER_PREFIX%_}"
    for br in $(ip -o link show type bridge 2>/dev/null | grep -oP 'br-[0-9a-f]+'); do
        iptables -D FORWARD -i "$br" -o "$br" -j "$iso_chain" 2>/dev/null || true
    done
    iptables -F "$iso_chain" 2>/dev/null || true
    iptables -X "$iso_chain" 2>/dev/null || true

    # Remove bridge
    if ip link show "$BRIDGE" &>/dev/null; then
        ip link set "$BRIDGE" down
        ip link delete "$BRIDGE"
        echo "Bridge ${BRIDGE} removed"
    fi

    # Remove leftover user-defined network bridges from the pool range
    cleanup_pool_bridges

    echo "=== All daemons stopped ==="
}
