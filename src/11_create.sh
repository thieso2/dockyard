cmd_create() {
    local INSTALL_SYSTEMD=true
    local START_DAEMON=true
    for arg in "$@"; do
        case "$arg" in
            --no-systemd) INSTALL_SYSTEMD=false ;;
            --no-start)   START_DAEMON=false ;;
            -h|--help)    create_usage ;;
            --*)          echo "Unknown option: $arg" >&2; create_usage ;;
        esac
    done

    require_root

    # Preflight: check for required system tools
    local missing=()
    for tool in curl iptables rsync; do
        if ! command -v "$tool" &>/dev/null; then
            missing+=("$tool")
        fi
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "Error: missing required tools: ${missing[*]}" >&2
        echo "Install them first, e.g.: apt-get install -y ${missing[*]}" >&2
        exit 1
    fi

    echo "Installing dockyard docker..."
    echo "  DOCKYARD_ROOT:          ${DOCKYARD_ROOT}"
    echo "  DOCKYARD_DOCKER_PREFIX: ${DOCKYARD_DOCKER_PREFIX}"
    echo "  DOCKYARD_BRIDGE_CIDR:   ${DOCKYARD_BRIDGE_CIDR}"
    echo "  DOCKYARD_FIXED_CIDR:    ${DOCKYARD_FIXED_CIDR}"
    echo "  DOCKYARD_POOL_BASE:     ${DOCKYARD_POOL_BASE}"
    echo "  DOCKYARD_POOL_SIZE:     ${DOCKYARD_POOL_SIZE}"
    if [ -n "${DOCKYARD_SYSBOX_MGR_EXTRA_ARGS:-}" ]; then
        echo "  sysbox-mgr extra args: ${DOCKYARD_SYSBOX_MGR_EXTRA_ARGS}"
    fi
    if [ -n "${DOCKYARD_SYSBOX_SUBID_START:-}${DOCKYARD_SYSBOX_SUBID_COUNT:-}" ]; then
        echo "  sysbox subid range:    ${DOCKYARD_SYSBOX_SUBID_USER}:${DOCKYARD_SYSBOX_SUBID_START}:${DOCKYARD_SYSBOX_SUBID_COUNT}"
    fi
    echo ""
    echo "  bridge:      ${BRIDGE}"
    echo "  service:     ${SERVICE_NAME}.service"
    echo "  root:        ${DOCKYARD_ROOT}"
    echo "  data:        ${DOCKER_DATA}"
    echo "  socket:      ${DOCKER_SOCKET}"
    echo "  user:        ${INSTANCE_USER}"
    echo "  group:       ${INSTANCE_GROUP}"
    echo ""

    # --- Check for existing installation ---
    check_private_cidr "$DOCKYARD_BRIDGE_CIDR" "DOCKYARD_BRIDGE_CIDR" || exit 1
    check_private_cidr "$DOCKYARD_FIXED_CIDR"  "DOCKYARD_FIXED_CIDR"  || exit 1
    check_private_cidr "$DOCKYARD_POOL_BASE"   "DOCKYARD_POOL_BASE"   || exit 1
    check_root_conflict "$DOCKYARD_ROOT" || exit 1
    check_prefix_conflict "$DOCKYARD_DOCKER_PREFIX" || exit 1
    check_subnet_conflict "$DOCKYARD_FIXED_CIDR" "$DOCKYARD_POOL_BASE" || exit 1

    # --- 1. Download and extract binaries ---
    local CACHE_DIR="${SCRIPT_DIR}/.tmp"

    # Version compatibility notes — do not upgrade these without reading:
    #
    # DOCKER_VERSION: static binary from download.docker.com/linux/static/stable.
    #   Uses sysbox-runc as default runtime → the bundled runc 1.3.3 is never
    #   called for sandbox containers, so this version does NOT trigger the
    #   sysbox procfs incompatibility (nestybox/sysbox#973).
    #
    # SYSBOX_VERSION: 0.7.0.6-tc is a patched fork (github.com/thieso2/sysbox)
    #   that adds --run-dir to sysbox-mgr, sysbox-fs, and sysbox-runc, allowing
    #   N independent sysbox instances per host (each with its own socket dir).
    #   SetRunDir() calls os.Setenv("SYSBOX_RUN_DIR", dir) and os.Args is scanned
    #   directly in init() — bypasses urfave/cli v1 so --run-dir via runtimeArgs
    #   works correctly for all three sockets including the seccomp tracer.
    #   No wrapper script needed.
    #   Fixed: https://github.com/thieso2/sysbox/issues/5
    #   Distributed as a static tarball (no .deb, no dpkg dependency).
    #   0.7.0.6-tc includes the netns regression fix tracked in:
    #   https://github.com/thieso2/sysbox/issues/9

    # --- Detect CPU architecture ---
    local ARCH
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|aarch64) ;;
        *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
    esac
    echo "  arch:        ${ARCH}"
    echo ""

    local DOCKER_VERSION="29.2.1"
    local DOCKER_ROOTLESS_VERSION="29.2.1"
    local SYSBOX_VERSION="0.7.0.6-tc"
    local SYSBOX_TARBALL="sysbox-static-${ARCH}.tar.gz"
    local COMPOSE_VERSION="2.32.4"

    # SHA256 checksums — must match exactly; cache hits are also verified
    # (protects against cache poisoning and mirror tampering)
    local DOCKER_SHA256 DOCKER_ROOTLESS_SHA256 SYSBOX_SHA256 COMPOSE_SHA256
    case "$ARCH" in
        x86_64)
            DOCKER_SHA256="995b1d0b51e96d551a3b49c552c0170bc6ce9f8b9e0866b8c15bbc67d1cf93a3"
            DOCKER_ROOTLESS_SHA256="8c7b7783d8b391ca3183d9b5c7dea1794f6de69cfaa13c45f61fcd17d2b9c3ef"
            SYSBOX_SHA256="91f44ab16948a14c4df8225d254e730e616b952a74879eb0a874692690fae20b"
            COMPOSE_SHA256="ed1917fb54db184192ea9d0717bcd59e3662ea79db48bff36d3475516c480a6b"
            ;;
        aarch64)
            DOCKER_SHA256="236c5064473295320d4bf732fbbfc5b11b6b2dc446e8bc7ebb9222015fb36857"
            DOCKER_ROOTLESS_SHA256="15895df8b46ff33179d357e61b600b5b51242f9b9587c0f66695689e62f57894"
            SYSBOX_SHA256="9601a03ab1455bf3a3409c7cc09df864df8c717c38e35f0c13ded80665b89d81"
            COMPOSE_SHA256="0c4591cf3b1ed039adcd803dbbeddf757375fc08c11245b0154135f838495a2f"
            ;;
    esac

    local DOCKER_URL="https://download.docker.com/linux/static/stable/${ARCH}/docker-${DOCKER_VERSION}.tgz"
    local DOCKER_ROOTLESS_URL="https://download.docker.com/linux/static/stable/${ARCH}/docker-rootless-extras-${DOCKER_ROOTLESS_VERSION}.tgz"
    local SYSBOX_URL="https://github.com/thieso2/sysbox/releases/download/v${SYSBOX_VERSION}/${SYSBOX_TARBALL}"
    local COMPOSE_URL="https://github.com/docker/compose/releases/download/v${COMPOSE_VERSION}/docker-compose-linux-${ARCH}"

    mkdir -p "$LOG_DIR" "$RUN_DIR" "$ETC_DIR" "$BIN_DIR"
    mkdir -p "$DOCKER_DATA" "$DOCKER_CONFIG_DIR"
    mkdir -p "${RUN_DIR}/containerd"
    mkdir -p "$CACHE_DIR"
    mkdir -p "$SYSBOX_RUN_DIR"
    mkdir -p "$SYSBOX_DATA_DIR"

    # Create system user and group for this instance.
    # dockerd runs as root but creates the socket owned by this group (--group flag),
    # so operators simply join the group to get socket access without sudo.
    if ! getent group "${INSTANCE_GROUP}" &>/dev/null; then
        groupadd --system "${INSTANCE_GROUP}"
        echo "  Created group ${INSTANCE_GROUP}"
    else
        echo "  Group ${INSTANCE_GROUP} already exists"
    fi
    if ! getent passwd "${INSTANCE_USER}" &>/dev/null; then
        useradd --system --no-create-home --shell /bin/false \
            --gid "${INSTANCE_GROUP}" "${INSTANCE_USER}"
        echo "  Created user ${INSTANCE_USER}"
    else
        echo "  User ${INSTANCE_USER} already exists"
    fi

    configure_sysbox_subids

    # Allow sysbox-fs FUSE mounts at this instance's sysbox mountpoint.
    # The default fusermount3 AppArmor profile (tightened in Ubuntu 25.10+)
    # only permits FUSE mounts under $HOME, /mnt, /tmp, etc.  Without this
    # override every sysbox container fails with a context-deadline-exceeded
    # RPC error from sysbox-fs.
    # Each instance appends a tagged block; destroy removes it.
    if [ -d /etc/apparmor.d ]; then
        mkdir -p /etc/apparmor.d/local
        local apparmor_file="/etc/apparmor.d/local/fusermount3"
        local apparmor_begin="# dockyard:${DOCKYARD_DOCKER_PREFIX}:begin"
        local apparmor_end="# dockyard:${DOCKYARD_DOCKER_PREFIX}:end"
        {
            flock -x 9
            if ! grep -qF "$apparmor_begin" "$apparmor_file" 2>/dev/null; then
                {
                    echo "$apparmor_begin"
                    # Ubuntu 25.10+ comments out dac_override in the base fusermount3
                    # profile (LP: #2122161). sysbox-fs needs it for FUSE mounts.
                    echo "capability dac_override,"
                    echo "mount fstype=fuse options=(nosuid,nodev) options in (ro,rw) -> ${SYSBOX_DATA_DIR}/**/,"
                    echo "umount ${SYSBOX_DATA_DIR}/**/,"
                    echo "$apparmor_end"
                } >> "$apparmor_file"
            fi
        } 9>"${apparmor_file}.lock"
        if [ -f /etc/apparmor.d/fusermount3 ]; then
            apparmor_parser -r /etc/apparmor.d/fusermount3
            echo "  AppArmor fusermount3 profile updated for ${SYSBOX_DATA_DIR}"
        fi
    fi

    verify_checksum() {
        local file="$1" expected="$2" name="$3"
        local actual
        actual=$(sha256sum "$file" | awk '{print $1}')
        if [ "$actual" != "$expected" ]; then
            echo "Error: SHA256 mismatch for $name" >&2
            echo "  expected: $expected" >&2
            echo "  got:      $actual" >&2
            rm -f "$file"
            exit 1
        fi
    }

    download() {
        local url="$1"
        local expected_sha256="$2"
        local dest="${CACHE_DIR}/$(basename "$url")"
        if [ -f "$dest" ]; then
            echo "  cached: $(basename "$dest")"
        else
            echo "  downloading: $(basename "$url")"
            curl -fsSL -o "${dest}.tmp.$$" "$url" && mv "${dest}.tmp.$$" "$dest"
        fi
        verify_checksum "$dest" "$expected_sha256" "$(basename "$url")"
    }

    echo "Downloading artifacts..."
    download "$DOCKER_URL"          "$DOCKER_SHA256"
    download "$DOCKER_ROOTLESS_URL" "$DOCKER_ROOTLESS_SHA256"
    download "$SYSBOX_URL"          "$SYSBOX_SHA256"
    download "$COMPOSE_URL"         "$COMPOSE_SHA256"

    # Use per-PID staging dirs for extraction so concurrent creates don't race
    # on a shared extraction directory (all instances share the same CACHE_DIR).
    local STAGING="${CACHE_DIR}/staging-$$"
    mkdir -p "$STAGING"
    trap 'rm -rf "$STAGING"' RETURN INT TERM

    echo "Extracting Docker binaries..."
    tar -xzf "${CACHE_DIR}/docker-${DOCKER_VERSION}.tgz" -C "$STAGING"
    cp -f "${STAGING}/docker/"* "$BIN_DIR/"

    echo "Extracting Docker rootless extras..."
    tar -xzf "${CACHE_DIR}/docker-rootless-extras-${DOCKER_ROOTLESS_VERSION}.tgz" -C "$STAGING"
    cp -f "${STAGING}/docker-rootless-extras/"* "$BIN_DIR/"

    echo "Extracting sysbox static binaries..."
    local SYSBOX_EXTRACT="${STAGING}/sysbox-static-${SYSBOX_VERSION}"
    mkdir -p "$SYSBOX_EXTRACT"
    tar -xzf "${CACHE_DIR}/${SYSBOX_TARBALL}" -C "$SYSBOX_EXTRACT"
    # All three sysbox binaries go directly to BIN_DIR.
    # sysbox-runc 0.6.7.9-tc parses --run-dir directly from os.Args in init(),
    # bypassing urfave/cli v1 entirely. runtimeArgs in daemon.json now works.
    # See: https://github.com/thieso2/sysbox/issues/5
    for bin in sysbox-runc sysbox-mgr sysbox-fs; do
        local src
        src=$(find "$SYSBOX_EXTRACT" -name "$bin" -type f | head -1)
        if [ -z "$src" ]; then
            echo "Error: $bin not found in ${SYSBOX_TARBALL}" >&2
            exit 1
        fi
        cp -f "$src" "$BIN_DIR/$bin"
        chmod +x "$BIN_DIR/$bin"
    done

    # Install Docker Compose v2 plugin
    echo "Installing Docker Compose plugin..."
    mkdir -p "${DOCKER_CONFIG_DIR}/cli-plugins"
    cp -f "${CACHE_DIR}/docker-compose-linux-${ARCH}" "${DOCKER_CONFIG_DIR}/cli-plugins/docker-compose"
    chmod +x "${DOCKER_CONFIG_DIR}/cli-plugins/docker-compose"

    chmod +x "$BIN_DIR"/*

    # Rename docker CLI binary, replace with DOCKER_HOST wrapper
    mv -f "${BIN_DIR}/docker" "${BIN_DIR}/docker-cli"
    cat > "${BIN_DIR}/docker" <<DOCKEREOF
#!/bin/bash
export DOCKER_HOST="unix://${DOCKER_SOCKET}"
export DOCKER_CONFIG="${DOCKER_CONFIG_DIR}"
exec "\$(dirname "\$0")/docker-cli" "\$@"
DOCKEREOF
    chmod +x "${BIN_DIR}/docker"

    echo "Installed binaries to ${BIN_DIR}/"

    # Detect storage driver and backing filesystem.
    # sysbox requires overlay2 — ZFS native driver causes "unknown fs" errors.
    # overlay2 works on ZFS 2.2+ with overlayfs kernel support.
    local STORAGE_DRIVER BACKING_FS
    STORAGE_DRIVER=$(detect_storage_driver "$DOCKER_DATA")
    BACKING_FS=$(detect_backing_fs "$DOCKER_DATA")
    echo "  storage:     ${STORAGE_DRIVER} (on ${BACKING_FS})"
    echo ""

    # Detect host upstream DNS so containers don't fall back to Docker's
    # hardcoded 8.8.8.8 when /etc/resolv.conf points at systemd-resolved.
    # See https://github.com/thieso2/dockyard/issues/19.
    local DNS_JSON="" dns_list dns_ip dns_joined=""
    dns_list=$(detect_upstream_dns)
    if [ -n "$dns_list" ]; then
        for dns_ip in $dns_list; do
            if [ -z "$dns_joined" ]; then
                dns_joined="\"${dns_ip}\""
            else
                dns_joined="${dns_joined},\"${dns_ip}\""
            fi
        done
        DNS_JSON="  \"dns\": [${dns_joined}],"$'\n'
        echo "  dns:         ${dns_list}"
    else
        echo "  dns:         (none detected — Docker will use built-in fallback)"
    fi

    # Write daemon.json (embedded — no external file dependency)
    # sysbox-runc 0.6.7.9-tc parses --run-dir from os.Args in init(), so
    # runtimeArgs works correctly. No wrapper script needed.
    cat > "${ETC_DIR}/daemon.json" <<DAEMONJSONEOF
{
  "default-runtime": "sysbox-runc",
  "runtimes": {
    "sysbox-runc": {
      "path": "${BIN_DIR}/sysbox-runc",
      "runtimeArgs": ["--run-dir", "${SYSBOX_RUN_DIR}"]
    }
  },
  "storage-driver": "${STORAGE_DRIVER}",
  "userland-proxy-path": "${BIN_DIR}/docker-proxy",
${DNS_JSON}  "features": {
    "buildkit": true
  }
}
DAEMONJSONEOF
    echo "Installed config to ${ETC_DIR}/daemon.json"

    # Copy config file and dockyard.sh into instance.
    # Installing as dockyard.sh means the script's own ../etc/dockyard.env
    # auto-discovery works: ${BIN_DIR}/dockyard.sh finds ${ETC_DIR}/dockyard.env
    # without needing DOCKYARD_ENV to be set.
    cp "$LOADED_ENV_FILE" "${ETC_DIR}/dockyard.env"
    cp "${SCRIPT_DIR}/dockyard.sh" "${BIN_DIR}/dockyard.sh"
    chmod +x "${BIN_DIR}/dockyard.sh"
    ln -sf dockyard.sh "${BIN_DIR}/dockyardctl"
    echo "Installed env to ${ETC_DIR}/dockyard.env"
    echo "Installed dockyard.sh to ${BIN_DIR}/dockyard.sh"

    # Set ownership of the instance root so every file is attributed to the
    # instance user/group. dockerd still runs as root, so it can write freely;
    # the ownership is for identification and directory-level access control.
    chown -R "${INSTANCE_USER}:${INSTANCE_GROUP}" "${DOCKYARD_ROOT}"
    echo "Set ownership of ${DOCKYARD_ROOT}/ to ${INSTANCE_USER}:${INSTANCE_GROUP}"

    # --- 2. Install systemd service ---
    if [ "$INSTALL_SYSTEMD" = true ]; then
        echo ""
        cmd_enable
    fi

    # --- 3. Start daemon ---
    if [ "$START_DAEMON" = true ]; then
        echo ""
        if [ "$INSTALL_SYSTEMD" = true ]; then
            echo "Starting via systemd..."
            systemctl start "${SERVICE_NAME}.service"
            echo "  ${SERVICE_NAME}.service started"
        else
            echo "Starting directly..."
            cmd_start
        fi
    fi

    echo ""
    echo "=== Installation complete ==="
    echo ""
    echo "To use:"
    echo "  ${BIN_DIR}/docker run -ti alpine ash"
    echo ""
    echo "Manage this instance:"
    echo "  ${BIN_DIR}/dockyard.sh status"
    echo "  sudo ${BIN_DIR}/dockyard.sh verify"
    echo "  sudo ${BIN_DIR}/dockyard.sh destroy"
}
