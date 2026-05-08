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

# Detect the host's upstream DNS resolvers for embedding into daemon.json.
# Fixes https://github.com/thieso2/dockyard/issues/19: on hosts where
# /etc/resolv.conf points at 127.0.0.53 (systemd-resolved), Docker detects
# loopback-only resolvers and falls back to hardcoded 8.8.8.8 / 8.8.4.4.
# Environments that block public DNS (e.g. Hetzner) then see silent DNS
# failure inside containers.
#
# Lookup order:
#   1. DOCKYARD_DNS env override (space- or comma-separated IPs)
#   2. resolvectl dns (systemd-resolved authoritative source)
#   3. /run/systemd/resolve/resolv.conf (real upstreams when resolved is used)
#   4. /etc/resolv.conf (whatever is there, loopback filtered out)
#
# Loopback (127.0.0.0/8) and link-local (169.254.0.0/16) entries are stripped.
# Returns a space-separated list on stdout, or empty string when nothing is
# available (caller then omits the "dns" key from daemon.json so Docker uses
# its own defaults).
detect_upstream_dns() {
    local raw=""

    if [ -n "${DOCKYARD_DNS:-}" ]; then
        raw="${DOCKYARD_DNS//,/ }"
    elif command -v resolvectl &>/dev/null; then
        # resolvectl dns prints "Global: 1.1.1.1 8.8.8.8" and per-link lines.
        # Extract every IP-shaped token from all lines.
        raw=$(resolvectl dns 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | sort -u | tr '\n' ' ')
    fi

    if [ -z "$raw" ] && [ -f /run/systemd/resolve/resolv.conf ]; then
        raw=$(awk '/^nameserver/ {print $2}' /run/systemd/resolve/resolv.conf | tr '\n' ' ')
    fi

    if [ -z "$raw" ] && [ -f /etc/resolv.conf ]; then
        raw=$(awk '/^nameserver/ {print $2}' /etc/resolv.conf | tr '\n' ' ')
    fi

    local ip out=""
    for ip in $raw; do
        case "$ip" in
            127.*|169.254.*|::1|fe80:*) continue ;;
        esac
        out="${out}${ip} "
    done
    echo "${out% }"
}

validate_uint() {
    local value="$1"
    local label="$2"

    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "Error: ${label} must be a positive integer (got '${value}')" >&2
        return 1
    fi
    if (( 10#$value == 0 )); then
        echo "Error: ${label} must be greater than zero" >&2
        return 1
    fi
}

configure_subid_file() {
    local path="$1"
    local user="$2"
    local start="$3"
    local count="$4"
    local tmp

    touch "$path"
    chmod 0644 "$path"

    {
        flock -x 9
        tmp=$(mktemp "${path}.dockyard.XXXXXX")
        awk -F: -v user="$user" '$1 != user { print }' "$path" > "$tmp"
        printf '%s:%s:%s\n' "$user" "$start" "$count" >> "$tmp"
        chmod 0644 "$tmp"
        mv "$tmp" "$path"
    } 9>"${path}.lock"
}

configure_sysbox_subids() {
    local start="${DOCKYARD_SYSBOX_SUBID_START:-}"
    local count="${DOCKYARD_SYSBOX_SUBID_COUNT:-}"
    local user="${DOCKYARD_SYSBOX_SUBID_USER:-$INSTANCE_USER}"

    if [ -z "${start}${count}" ]; then
        return 0
    fi
    if [ -z "$start" ] || [ -z "$count" ]; then
        echo "Error: DOCKYARD_SYSBOX_SUBID_START and DOCKYARD_SYSBOX_SUBID_COUNT must be set together." >&2
        exit 1
    fi
    if [ -z "$user" ]; then
        echo "Error: DOCKYARD_SYSBOX_SUBID_USER must not be empty." >&2
        exit 1
    fi

    validate_uint "$start" "DOCKYARD_SYSBOX_SUBID_START" || exit 1
    validate_uint "$count" "DOCKYARD_SYSBOX_SUBID_COUNT" || exit 1

    configure_subid_file /etc/subuid "$user" "$start" "$count"
    configure_subid_file /etc/subgid "$user" "$start" "$count"
    echo "  Configured subuid/subgid ${user}:${start}:${count}"
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
