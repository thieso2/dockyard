# ── Usage ────────────────────────────────────────────────────

usage() {
    cat <<'EOF'
Usage: ./dockyard.sh <command> [options]

Commands:
  gen-env     Generate a conflict-free dockyard.env config file
  create      Download binaries, install config, set up systemd, start daemon
  enable      Install systemd service for this instance
  disable     Remove systemd service for this instance
  start       Start daemons manually (without systemd)
  stop        Stop manually started daemons
  status      Show instance status
  verify      Smoke-test a running instance (service, containers, DinD, networking)
  destroy     Stop and remove everything

All commands except gen-env require a config file:
  1. $DOCKYARD_ENV (if set)
  2. ./dockyard.env (in current directory)
  3. ../etc/dockyard.env (relative to script — for installed copy)
  4. $DOCKYARD_ROOT/etc/dockyard.env

Examples:
  ./dockyard.sh gen-env
  sudo ./dockyard.sh create
  sudo ./dockyard.sh create --no-systemd --no-start
  sudo ./dockyard.sh start
  sudo ./dockyard.sh stop
  ./dockyard.sh status
  sudo ./dockyard.sh verify
  sudo ./dockyard.sh destroy
  sudo ./dockyard.sh destroy --keep-data   # preserve container data

  # Multiple instances
  DOCKYARD_DOCKER_PREFIX=test_ DOCKYARD_ROOT=/test ./dockyard.sh gen-env
  DOCKYARD_ENV=./dockyard.env sudo -E ./dockyard.sh create
EOF
    exit 0
}

gen_env_usage() {
    cat <<'EOF'
Usage: ./dockyard.sh gen-env [OPTIONS]

Generate a dockyard.env config file with randomized, conflict-free networks.

Options:
  --nocheck     Skip all conflict checks
  -h, --help    Show this help

Output: ./dockyard.env (or $DOCKYARD_ENV if set). Errors if file exists.

Override any variable via environment:
  DOCKYARD_ROOT           Installation root (default: /dockyard)
  DOCKYARD_DOCKER_PREFIX  Prefix for bridge/service (default: dy_)
  DOCKYARD_BRIDGE_CIDR    Bridge IP/mask (default: random from 172.16.0.0/12)
  DOCKYARD_FIXED_CIDR     Container subnet (default: derived from bridge)
  DOCKYARD_POOL_BASE      Address pool base (default: random from 172.16.0.0/12)
  DOCKYARD_POOL_SIZE      Pool subnet size (default: 24)

Examples:
  ./dockyard.sh gen-env
  DOCKYARD_DOCKER_PREFIX=test_ ./dockyard.sh gen-env
  DOCKYARD_ROOT=/docker2 DOCKYARD_DOCKER_PREFIX=d2_ ./dockyard.sh gen-env
  ./dockyard.sh gen-env --nocheck
EOF
    exit 0
}

create_usage() {
    cat <<'EOF'
Usage: sudo ./dockyard.sh create [OPTIONS]

Create a dockyard instance: download binaries, install config,
set up systemd service, and start the daemon.

Requires a dockyard.env config file (run gen-env first).

Options:
  --no-systemd    Skip systemd service installation
  --no-start      Don't start the daemon after install
  -h, --help      Show this help

Examples:
  ./dockyard.sh gen-env && sudo ./dockyard.sh create
  sudo ./dockyard.sh create --no-systemd --no-start
  DOCKYARD_ENV=./custom.env sudo -E ./dockyard.sh create
EOF
    exit 0
}
