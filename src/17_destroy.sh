cmd_destroy() {
    local YES=false
    local KEEP_DATA=false
    for arg in "$@"; do
        case "$arg" in
            --yes|-y)       YES=true ;;
            --keep-data|-k) KEEP_DATA=true ;;
            -h|--help)      usage ;;
        esac
    done

    require_root

    local SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

    if [[ "$KEEP_DATA" == true ]]; then
        echo "This will remove the dockyard instance (binaries and config only — data preserved):"
        echo "  ${SERVICE_FILE}"
        echo "  ${DOCKYARD_ROOT}/  (except ${DOCKER_DATA}/)"
    else
        echo "This will remove all installed dockyard docker files:"
        echo "  ${SERVICE_FILE}    (docker systemd service)"
        echo "  ${DOCKYARD_ROOT}/  (all instance data: binaries, config, data, logs, sockets)"
        local data_size
        data_size=$(du -sh "${DOCKER_DATA}" 2>/dev/null | cut -f1 || echo "unknown")
        echo ""
        echo "Warning: this will permanently delete container data:"
        echo "  ${DOCKER_DATA}/  (~${data_size})"
        echo "  Use --keep-data (-k) to preserve container data."
    fi
    echo ""
    if [[ "$YES" != true ]]; then
        read -p "Continue? [y/N] " confirm
        if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
            echo "Aborted."
            exit 0
        fi
    fi

    # --- 1. Stop and remove systemd service (or stop daemons directly) ---
    if [ -f "$SERVICE_FILE" ]; then
        cmd_disable
    else
        # No systemd service — stop daemons directly
        for pidfile in "${RUN_DIR}/dockerd.pid" "${RUN_DIR}/containerd.pid"; do
            if [ -f "$pidfile" ]; then
                local pid
                pid=$(cat "$pidfile")
                if kill -0 "$pid" 2>/dev/null; then
                    echo "Stopping pid ${pid}..."
                    kill "$pid" 2>/dev/null || true
                fi
                rm -f "$pidfile"
            fi
        done
        stop_daemon sysbox-fs "${SYSBOX_RUN_DIR}/sysbox-fs.pid" 10
        stop_daemon sysbox-mgr "${SYSBOX_RUN_DIR}/sysbox-mgr.pid" 10
        rm -rf "$SYSBOX_RUN_DIR"
        rm -f "$DOCKER_SOCKET" "$CONTAINERD_SOCKET"
        if ip link show "$BRIDGE" &>/dev/null; then
            ip link set "$BRIDGE" down 2>/dev/null || true
            ip link delete "$BRIDGE" 2>/dev/null || true
        fi
        sleep 2
    fi

    # --- 1.5. Remove leftover user-defined network bridges from the pool ---
    cleanup_pool_bridges

    # --- 2. Remove AppArmor fusermount3 entry for this instance ---
    local apparmor_file="/etc/apparmor.d/local/fusermount3"
    local apparmor_begin="# dockyard:${DOCKYARD_DOCKER_PREFIX}:begin"
    if grep -qF "$apparmor_begin" "$apparmor_file" 2>/dev/null; then
        {
            flock -x 9
            awk -v start="$apparmor_begin" \
                -v stop="# dockyard:${DOCKYARD_DOCKER_PREFIX}:end" \
                '$0 == start { skip=1 } skip { if ($0 == stop) { skip=0 }; next } { print }' \
                "$apparmor_file" > "${apparmor_file}.tmp" \
                && mv "${apparmor_file}.tmp" "$apparmor_file"
        } 9>"${apparmor_file}.lock"
        if [ -f /etc/apparmor.d/fusermount3 ]; then
            apparmor_parser -r /etc/apparmor.d/fusermount3 2>/dev/null || true
        fi
        echo "Removed AppArmor fusermount3 entry for ${DOCKYARD_DOCKER_PREFIX}"
    fi

    # --- 3. Remove instance root (selective or full) ---
    if [ -d "$DOCKYARD_ROOT" ]; then
        if [[ "$KEEP_DATA" == true ]]; then
            rm -rf "${DOCKYARD_ROOT}/bin"
            rm -rf "${DOCKYARD_ROOT}/etc"
            rm -rf "${DOCKYARD_ROOT}/log"
            rm -rf "${DOCKYARD_ROOT}/run"
            rm -rf "${DOCKYARD_ROOT}/lib/sysbox"
            rm -rf "${DOCKYARD_ROOT}/lib/docker-config"
            echo "Removed instance files from ${DOCKYARD_ROOT}/"
            echo "Data preserved at ${DOCKER_DATA}"
        else
            # When the data-root sits on a ZFS dataset, Docker (with overlay2)
            # may have created overlay dirs that need normal removal, and
            # rm -rf on a ZFS mountpoint removes contents but can't remove
            # the mountpoint directory itself.
            local on_zfs=false
            if command -v zfs &>/dev/null; then
                local root_dataset
                root_dataset="$(df --output=source "$DOCKYARD_ROOT" 2>/dev/null | tail -1 | tr -d '[:space:]')" || true
                if [ -n "$root_dataset" ] && zfs list "$root_dataset" &>/dev/null; then
                    on_zfs=true
                    # Destroy all child ZFS datasets (Docker layers, etc.)
                    local children
                    children="$(zfs list -r -H -o name "$root_dataset" 2>/dev/null | tail -n +2 | sort -r)" || true
                    if [ -n "$children" ]; then
                        echo "Cleaning up ZFS datasets under ${root_dataset}..."
                        while IFS= read -r ds; do
                            zfs destroy -f "$ds" 2>/dev/null || true
                        done <<< "$children"
                    fi
                fi
            fi
            if [ "$on_zfs" = true ]; then
                # Can't remove ZFS mountpoint itself; remove contents only
                rm -rf "${DOCKYARD_ROOT:?}"/* "${DOCKYARD_ROOT}"/.[!.]* 2>/dev/null || true
                echo "Removed contents of ${DOCKYARD_ROOT}/ (ZFS mountpoint preserved)"
            else
                rm -rf "$DOCKYARD_ROOT"
                echo "Removed ${DOCKYARD_ROOT}/"
            fi
        fi
    fi

    # --- 4. Remove instance user and group ---
    if getent passwd "${INSTANCE_USER}" &>/dev/null; then
        userdel "${INSTANCE_USER}" 2>/dev/null || true
        echo "Removed user ${INSTANCE_USER}"
    fi
    if getent group "${INSTANCE_GROUP}" &>/dev/null; then
        groupdel "${INSTANCE_GROUP}" 2>/dev/null || true
        echo "Removed group ${INSTANCE_GROUP}"
    fi

    echo ""
    echo "=== Uninstall complete ==="
}
