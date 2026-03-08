# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Dockyard: multi-instance Docker daemon installer with sysbox-runc as default runtime. Runs isolated Docker instances side-by-side with the system Docker on the same host. Each instance gets its own bridge network, containerd, socket, and data directory.

## Building

```bash
# Build dist/dockyard.sh from src/*.sh (concatenates in numeric order, strips per-file shebangs)
./build.sh

# Build the integration test binary (run from repo root)
GOOS=linux GOARCH=amd64 go build -o cmd/dockyardtest/dockyardtest_linux ./cmd/dockyardtest/
GOOS=linux GOARCH=arm64 go build -o cmd/dockyardtest/dockyardtest_linux_arm64 ./cmd/dockyardtest/
GOOS=darwin GOARCH=arm64 go build -o cmd/dockyardtest/dockyardtest_mac ./cmd/dockyardtest/
```

The `dockyard.sh` at the repo root is the working copy (not a symlink). `dist/dockyard.sh` is the built output. Edit `src/*.sh` and run `./build.sh` — never edit `dist/dockyard.sh` directly.

## Running the Integration Tests

The test suite (`cmd/dockyardtest/main.go`) SSHes into a remote Linux VM and runs 29 end-to-end tests across 3 dockyard instances.

```bash
# Upload dist + binary to target host, then run
./dockyardtest_mac --host HOST --user thies [--key /path/to/key]

# Run a specific test by name (substring match on test subject)
./dockyardtest_mac --host HOST --user thies --run "verify"
```

See MEMORY.md for known test VM addresses and cross-host SSH key setup.

## Key Commands

```bash
# Generate a config with randomized networks
./dockyard.sh gen-env
DOCKYARD_DOCKER_PREFIX=test_ DOCKYARD_ROOT=/test ./dockyard.sh gen-env

# Create instance (requires dockyard.env)
sudo ./dockyard.sh create
sudo ./dockyard.sh create --no-systemd --no-start

# Create with explicit env file
DOCKYARD_ENV=./custom.env sudo -E ./dockyard.sh create

# Post-create (reads ./dockyard.env or $DOCKYARD_ENV)
sudo ./dockyard.sh start
sudo ./dockyard.sh stop
./dockyard.sh status
sudo ./dockyard.sh destroy
```

Using a custom instance:
```bash
DOCKER_HOST=unix:///dockyard/run/docker.sock docker ps
```

## Architecture

### Single Script, Subcommand Interface

Everything lives in `dockyard.sh` with subcommands: `gen-env`, `create`, `enable`, `disable`, `start`, `stop`, `status`, `destroy`. The script is fully self-contained: embedded daemon.json, no external file dependencies.

### Environment Loading

All commands except `gen-env` require a config file (mandatory, no silent fallback):

1. If `DOCKYARD_ENV` is set → source that file (error if missing)
2. Else if `./dockyard.env` exists in current directory → source it
3. Else if `../etc/dockyard.env` exists relative to the script → source it (installed copy at `$BIN_DIR/dockyard.sh` finds `$ETC_DIR/dockyard.env`)
4. Else if `$DOCKYARD_ROOT/etc/dockyard.env` exists → source it
5. Otherwise → error: `"No config found. Run './dockyard.sh gen-env' or set DOCKYARD_ENV."`

The `gen-env` command creates the config file. It does not go through `load_env()`.

### Environment Variables

Each env file defines 6 variables that fully configure an instance:

| Variable | Purpose | Must be unique per instance |
|----------|---------|----------------------------|
| `DOCKYARD_ROOT` | Base directory for data/runtime/socket | Yes |
| `DOCKYARD_DOCKER_PREFIX` | Prefix for bridge, service name | Yes |
| `DOCKYARD_BRIDGE_CIDR` | Bridge IP/mask (e.g. `172.22.147.1/24`) | Yes |
| `DOCKYARD_FIXED_CIDR` | Container subnet (e.g. `172.22.147.0/24`) | Yes |
| `DOCKYARD_POOL_BASE` | Address pool for user networks | Yes |
| `DOCKYARD_POOL_SIZE` | Pool subnet size in CIDR bits | No |

Everything else is derived: `BIN_DIR`, `ETC_DIR`, `LOG_DIR`, `RUN_DIR`, `BRIDGE`, `SERVICE_NAME`, `DOCKER_SOCKET`, `CONTAINERD_SOCKET`, `DOCKER_DATA`, `DOCKER_CONFIG_DIR`, `INSTANCE_USER`, `INSTANCE_GROUP`, `SYSBOX_RUN_DIR`, `SYSBOX_DATA_DIR`.

### gen-env: Config Generation

`gen-env` generates a `dockyard.env` file with conflict-free randomized networks:

- Picks random /24 from `172.16.0.0/12` for bridge CIDR
- Picks random /16 from `172.16.0.0/12` (different second octet) for pool base
- Validates against `ip route`, retries up to 10 times on collision
- Checks prefix conflicts (bridge, systemd service)
- Checks root dir conflicts (existing installation at `${root}/bin`)
- All checks skippable with `--nocheck`
- All 6 variables overridable via environment

### Collision Checks (Shared Helpers)

Three reusable helpers used by both `gen-env` and `create`:

- `check_prefix_conflict()` — bridge exists, docker systemd service exists
- `check_root_conflict()` — `bin/` already present at the given root
- `check_subnet_conflict()` — `ip route` overlap for fixed CIDR and pool base

### Downloaded Software

Defined in `cmd_create()`, cached in `.tmp/`:

| Software | Version | Source |
|----------|---------|--------|
| Docker CE (static) | 29.2.1 | download.docker.com |
| Docker Rootless Extras | 29.2.1 | download.docker.com |
| Docker Compose v2 | 2.32.4 | github.com/docker/compose |
| Sysbox (fork, static tarball) | 0.6.7.10-tc | github.com/thieso2/sysbox |

The fork ships as a static tarball containing all three binaries (`sysbox-mgr`, `sysbox-fs`, `sysbox-runc`).

### Per-Instance Sysbox Daemon (0.6.7.9-tc fork)

The patched fork (`github.com/thieso2/sysbox`) adds `--run-dir` to all three sysbox binaries, allowing N independent sysbox instances per host. `SetRunDir()` calls `os.Setenv("SYSBOX_RUN_DIR", dir)`, so `runtimeArgs: ["--run-dir", "..."]` in daemon.json works correctly. No wrapper script needed.

**Derived variables** (set in `derive_vars()`):
- `SYSBOX_RUN_DIR="${DOCKYARD_ROOT}/run/sysbox"` — sockets + PID files
- `SYSBOX_DATA_DIR="${DOCKYARD_ROOT}/lib/sysbox"` — sysbox-mgr data-root and sysbox-fs mountpoint

**Startup Sequence**:
```
${PREFIX}docker.service starts (per instance)
  ExecStartPre: sysbox-mgr starts with --run-dir ${SYSBOX_RUN_DIR} --data-root ${SYSBOX_DATA_DIR}
  ExecStartPre: wait for ${SYSBOX_RUN_DIR}/sysmgr.sock
  ExecStartPre: sysbox-fs starts with --run-dir ${SYSBOX_RUN_DIR} --mountpoint ${SYSBOX_DATA_DIR}
  ExecStartPre: wait for ${SYSBOX_RUN_DIR}/sysfs.sock
  ExecStartPre: iptables rules inserted
  ExecStart: containerd
  ExecStart: dockerd (with --group ${INSTANCE_GROUP})
  ExecStopPost: iptables rules removed
  ExecStopPost: kill sysbox-fs
  ExecStopPost: kill sysbox-mgr
  ExecStopPost: rm -rf ${SYSBOX_RUN_DIR}
```

There is no shared sysbox service. No ref-counting. No `dockyard-sysbox.service`.

### Per-Instance User and Group

Each dockyard instance creates a dedicated system user and group at `create` time:

- User/group name: `${DOCKYARD_DOCKER_PREFIX}docker` (e.g. `dy1_docker`)
- Derived vars: `INSTANCE_USER="${DOCKYARD_DOCKER_PREFIX}docker"`, `INSTANCE_GROUP="${DOCKYARD_DOCKER_PREFIX}docker"`
- Ownership: `DOCKYARD_ROOT` is `chown -R ${INSTANCE_USER}:${INSTANCE_GROUP}` after install
- Socket access: `dockerd --group ${INSTANCE_GROUP}` makes the socket `root:${GROUP} 660`
- Users in the group can access the socket without `sudo`
- Both user and group are removed by `destroy`

### Self-Contained Systemd Services

The service file template expands all variables at create time. The generated `.service` file has no external dependencies on this repo's scripts. This is intentional — the service works even if this repo is deleted.

### Networking: Explicit iptables, Not Docker-Managed

Docker's built-in iptables management (`--iptables=true`) uses global chain names (`DOCKER-FORWARD`, `DOCKER-USER`, etc.) that get clobbered when multiple dockerd instances start. We disable it (`--iptables=false`) and manage iptables explicitly in the systemd service lifecycle:

- **ExecStartPre**: Inserts 3 FORWARD rules + 1 NAT MASQUERADE rule, all scoped to the instance's bridge name
- **ExecStopPost**: Removes the same rules

Each rule uses `-i $BRIDGE` or `-o $BRIDGE` so instances can never interfere with each other or with the system Docker.

### Isolation Rules (`isolation.d/`)

Consumers (e.g. Sandcastle) can drop `.rules` files into `${ETC_DIR}/isolation.d/` to enable intra-bridge traffic filtering. Each `.rules` file contains one IP per line to ACCEPT; all other traffic between containers on the same user-defined bridge is DROPped.

When `.rules` files exist, dockyard creates a per-instance `DOCKYARD-ISO-${PREFIX}` iptables chain (e.g. `DOCKYARD-ISO-dy1`) on start and inserts bridge-scoped jump rules (`-i br-xxx -o br-xxx -j DOCKYARD-ISO-dy1`) for every user-defined Docker network. On stop, the chain and all jump rules are cleaned up. Comments (`#`) and blank lines in `.rules` files are ignored.

This is a generic hook — dockyard itself never writes `.rules` files.

### Directory Layout (per instance)

```
${DOCKYARD_ROOT}/                        # owned by ${INSTANCE_USER}:${INSTANCE_GROUP}
├── bin/                                 # dockerd, containerd, sysbox-mgr, sysbox-fs,
│                                        # sysbox-runc, docker-cli, docker (wrapper), dockyard.sh (dockyardctl symlink)
├── etc/
│   ├── daemon.json                      # Docker daemon config
│   ├── dockyard.env                     # Copy of config (written by create)
│   └── isolation.d/                     # Optional: .rules files for DOCKYARD-ISOLATION chain
├── lib/
│   ├── docker/                          # Docker data-root (images, containers, volumes)
│   │   └── containerd/                  # containerd content store
│   ├── sysbox/                          # sysbox-mgr --data-root + sysbox-fs --mountpoint
│   └── docker-config/                   # DOCKER_CONFIG dir (credentials, config.json)
├── log/
│   ├── dockerd.log
│   ├── containerd.log
│   ├── sysbox-mgr.log
│   └── sysbox-fs.log
└── run/                                 # all runtime sockets + PIDs in one place
    ├── docker.sock                      # Docker API socket (root:${INSTANCE_GROUP} 660)
    ├── dockerd.pid
    ├── containerd.pid
    ├── containerd/
    │   └── containerd.sock
    └── sysbox/
        ├── sysmgr.sock
        ├── sysfs.sock
        ├── sysbox-mgr.pid
        └── sysbox-fs.pid

/etc/systemd/system/
└── ${PREFIX}docker.service              # Per-instance docker service (no shared sysbox service)

/etc/apparmor.d/local/fusermount3        # Per-instance tagged block, removed on destroy
```

## Key Files

- `build.sh` — concatenates `src/[0-9]*.sh` → `dist/dockyard.sh`; strips only the first-line shebang per file
- `src/00_header.sh` — shebang + `set -euo pipefail` + SCRIPT_DIR; must remain the first file
- `src/01_env.sh` — `load_env()` (5-step config discovery) and `derive_vars()` (all derived paths + user/group vars)
- `src/02_helpers.sh` — `require_root()`, `stop_daemon()`, `wait_for_file()`, `download()` (atomic fetch + SHA256)
- `src/03_checks.sh` — `check_prefix_conflict()`, `check_root_conflict()`, `check_subnet_conflict()`; shared by gen-env and create
- `src/10_gen_env.sh` — CIDR randomization, conflict retries, writes `dockyard.env`
- `src/11_create.sh` — groupadd/useradd, binary install (static tarball, not .deb), chown after install
- `src/12_enable.sh` — per-instance docker.service with inline sysbox ExecStartPre/ExecStopPost, `--group` flag for dockerd
- `src/13_disable.sh` — removes docker service only (no shared sysbox service logic)
- `src/14_start.sh` — starts sysbox-mgr and sysbox-fs inline before containerd/dockerd, `--group` flag
- `src/15_stop.sh` — stops dockerd, containerd, sysbox-fs, sysbox-mgr in order
- `src/16_status.sh` — reads PIDs from `run/*.pid`; uses `/proc/$pid` (no root needed)
- `src/17_destroy.sh` — `rm -rf DOCKYARD_ROOT`, AppArmor block removal, userdel/groupdel
- `src/18_verify.sh` — post-install smoke test (service, socket, API, container run, ping, DinD); all output checks use `grep -q`
- `src/90_usage.sh` — help text for each subcommand
- `src/99_dispatch.sh` — `case "$1"` router; entry point that calls `cmd_*` functions
- `cmd/dockyardtest/main.go` — Go integration test suite (29 tests, SSH-based, 3 instances)
- `ARCHITECTURE.md` — comprehensive design doc with mermaid diagrams
- `FINDINGS.md` — root cause analysis of all discovered issues
- `PROGRESS.md` — architecture summary and test phase breakdown

## Downstream: Sandcastle

Sandcastle (`thieso2/Sandcastle`) bundles a copy of dockyard at `installer/templates/dockyard.sh`. **Never fix dockyard bugs in the Sandcastle copy** — always fix them here in the dockyard repo first, then update the Sandcastle template to match. The Sandcastle template should only contain Sandcastle-specific additions (not divergent fixes to dockyard code).

## Script Conventions

- `set -euo pipefail`
- Env loading: `set -a; source "$ENV_FILE"; set +a`
- Operations are idempotent (bridge creation, iptables removal, socket cleanup)
- Binaries are cached in `.tmp/` to avoid re-downloading
- `status` works without root (uses `/proc/$pid` instead of `kill -0`)
- `build.sh` uses `awk 'NR==1 && /^#!/ {next} {print}'` to strip per-file shebangs — `grep -v '^#!'` would also strip `#!` lines inside heredocs
