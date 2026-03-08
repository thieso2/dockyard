# ── Helpers ──────────────────────────────────────────────────

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "Error: must run as root (use sudo)" >&2
        exit 1
    fi
}

stop_daemon() {
    local name="$1"
    local pidfile="$2"
    local timeout="${3:-10}"

    if [ ! -f "$pidfile" ]; then
        echo "${name}: no pid file"
        return 0
    fi

    local pid
    pid=$(cat "$pidfile")

    if ! kill -0 "$pid" 2>/dev/null; then
        echo "${name}: not running (stale pid ${pid})"
        rm -f "$pidfile"
        return 0
    fi

    echo "Stopping ${name} (pid ${pid})..."
    kill "$pid"

    local i=0
    while kill -0 "$pid" 2>/dev/null; do
        sleep 1
        i=$((i + 1))
        if [ "$i" -ge "$timeout" ]; then
            echo "  ${name} did not stop in ${timeout}s — sending SIGKILL"
            kill -9 "$pid" 2>/dev/null || true
            sleep 1
            break
        fi
    done

    rm -f "$pidfile"
    echo "  ${name} stopped"
}

cleanup_pool_bridges() {
    # Remove leftover kernel bridge interfaces (br-*) whose IP falls within
    # DOCKYARD_POOL_BASE. When dockerd exits, it does not clean up user-defined
    # network bridges. Left behind, they cause "overlaps with existing routes"
    # errors on the next install because the pool CIDR is still in the routing table.
    local pool_base="${DOCKYARD_POOL_BASE:-}"
    [ -n "$pool_base" ] || return 0

    # Extract the first two octets of the pool base (e.g. "10.89" from "10.89.0.0/16")
    local pool_prefix
    pool_prefix=$(echo "$pool_base" | grep -oP '^\d+\.\d+')

    local removed=0
    while IFS= read -r iface; do
        [[ "$iface" == br-* ]] || continue
        local iface_ip
        iface_ip=$(ip addr show "$iface" 2>/dev/null | grep -oP 'inet \K[^/]+' | head -1)
        if [[ -n "$iface_ip" && "$iface_ip" == ${pool_prefix}.* ]]; then
            echo "Removing leftover pool bridge: ${iface} (${iface_ip})"
            ip link set "$iface" down 2>/dev/null || true
            ip link delete "$iface" 2>/dev/null || true
            removed=$((removed + 1))
        fi
    done < <(ip link show type bridge 2>/dev/null | grep -oP '^\d+: \K[^:@]+')

    [ "$removed" -gt 0 ] || true
}

# Detect the backing filesystem type for the given data directory.
# Returns the fstype string (e.g., "ext4", "zfs", "xfs").
detect_backing_fs() {
    local data_dir="$1"

    # Walk up to the nearest existing directory
    local check_dir="$data_dir"
    while [ ! -d "$check_dir" ]; do
        check_dir="$(dirname "$check_dir")"
    done

    df --output=fstype "$check_dir" 2>/dev/null | tail -1 | tr -d '[:space:]'
}

# Detect the optimal Docker storage driver for the given data directory.
# Always returns "overlay2" — sysbox-runc does not support ZFS as a container
# rootfs filesystem (fails with "unknown fs"). overlay2 works on ZFS 2.2+ and
# on all other common Linux filesystems.
# Can be overridden with DOCKYARD_STORAGE_DRIVER for future use.
detect_storage_driver() {
    local data_dir="$1"

    # Manual override
    if [ -n "${DOCKYARD_STORAGE_DRIVER:-}" ]; then
        case "$DOCKYARD_STORAGE_DRIVER" in
            auto)   ;; # fall through to detection
            overlay2|zfs)
                echo "$DOCKYARD_STORAGE_DRIVER"
                return
                ;;
            *)
                echo "Error: unsupported DOCKYARD_STORAGE_DRIVER=${DOCKYARD_STORAGE_DRIVER} (use auto, overlay2, or zfs)" >&2
                exit 1
                ;;
        esac
    fi

    # sysbox requires overlay2 — it does not recognize ZFS rootfs.
    # overlay2 works on ZFS 2.2+ with overlayfs kernel support.
    echo "overlay2"
}

wait_for_file() {
    local file="$1"
    local label="$2"
    local timeout="${3:-30}"
    local i=0
    while [ ! -S "$file" ]; do
        sleep 1
        i=$((i + 1))
        if [ "$i" -ge "$timeout" ]; then
            echo "Error: $label did not become ready within ${timeout}s" >&2
            return 1
        fi
    done
}
