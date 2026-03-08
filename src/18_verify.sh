# ── Verify ───────────────────────────────────────────────────────────────────
# Smoke-tests a running dockyard instance: service, socket, API, containers,
# outbound networking, and Docker-in-Docker (sysbox).
# Exits 0 only when every check passes.

cmd_verify() {
    local _p=0 _f=0
    local _d="${BIN_DIR}/docker"
    local _s="unix://${DOCKER_SOCKET}"
    local out

    _pass() { echo "  PASS: $1"; _p=$((_p + 1)); }
    _fail() { echo "  FAIL: $1 — $2" >&2; _f=$((_f + 1)); }

    echo "=== dockyard verify: ${SERVICE_NAME} ==="
    echo ""

    # 1. systemd service active
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        _pass "systemd service ${SERVICE_NAME} active"
    else
        _fail "systemd service" "${SERVICE_NAME} is $(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo unknown)"
    fi

    # 2. docker socket exists
    if [ -S "${DOCKER_SOCKET}" ]; then
        _pass "docker socket exists"
    else
        _fail "docker socket" "${DOCKER_SOCKET} not found"
    fi

    # 3. docker info (API reachable)
    if DOCKER_HOST="$_s" "$_d" info >/dev/null 2>&1; then
        _pass "docker info (API reachable)"
    else
        out=$(DOCKER_HOST="$_s" "$_d" info 2>&1 | tail -2)
        _fail "docker info" "$out"
    fi

    # 4. basic container run
    out=$(DOCKER_HOST="$_s" "$_d" run --rm alpine echo verify-ok 2>&1)
    if echo "$out" | grep -q "verify-ok"; then
        _pass "container run (alpine echo)"
    else
        _fail "container run" "$out"
    fi

    # 5. outbound networking
    if DOCKER_HOST="$_s" "$_d" run --rm alpine ping -c3 -W2 1.1.1.1 >/dev/null 2>&1; then
        _pass "outbound networking (ping 1.1.1.1)"
    else
        _fail "outbound networking" "ping 1.1.1.1 failed from container"
    fi

    # 6. Docker-in-Docker via sysbox
    local cname="dockyard-verify-$$"
    DOCKER_HOST="$_s" "$_d" rm -f "$cname" >/dev/null 2>&1 || true
    # Mask /usr/sbin/zfs inside DinD to prevent the containerd ZFS snapshotter
    # probe from hanging for 10s when the outer filesystem is ZFS.
    if DOCKER_HOST="$_s" "$_d" run -d --name "$cname" -v /dev/null:/usr/sbin/zfs docker:26.1-dind >/dev/null 2>&1; then
        local ready=false i
        for i in $(seq 1 30); do
            if DOCKER_HOST="$_s" "$_d" exec "$cname" docker info >/dev/null 2>&1; then
                ready=true
                break
            fi
            sleep 2
        done
        if $ready; then
            # Preload alpine from host cache to avoid Docker Hub rate limits.
            if [ -f /var/tmp/alpine.tar ]; then
                DOCKER_HOST="$_s" "$_d" exec -i "$cname" docker load < /var/tmp/alpine.tar >/dev/null 2>&1 || true
            fi
            out=$(DOCKER_HOST="$_s" "$_d" exec "$cname" docker run --rm alpine echo dind-ok 2>&1)
            if echo "$out" | grep -q "dind-ok"; then
                _pass "Docker-in-Docker (inner container via sysbox)"
            else
                _fail "DinD inner container" "$out"
            fi
        else
            _fail "DinD" "inner dockerd not ready after 60s"
        fi
        DOCKER_HOST="$_s" "$_d" rm -f "$cname" >/dev/null 2>&1 || true
    else
        out=$(DOCKER_HOST="$_s" "$_d" run --name "$cname" -v /dev/null:/usr/sbin/zfs docker:26.1-dind 2>&1 | head -3)
        DOCKER_HOST="$_s" "$_d" rm -f "$cname" >/dev/null 2>&1 || true
        _fail "DinD" "could not start docker:26.1-dind — $out"
    fi

    echo ""
    if [ "$_f" -eq 0 ]; then
        echo "All ${_p} checks passed."
    else
        echo "${_p} passed, ${_f} failed."
    fi
    return "$_f"
}
