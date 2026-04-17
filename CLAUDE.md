# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Dockyard: multi-instance Docker daemon installer with sysbox-runc as default runtime. Runs isolated Docker instances side-by-side with the system Docker on the same host. Each instance gets its own bridge network, containerd, socket, and data directory.

## Building

```bash
# Build dist/dockyard.sh from src/*.sh (concatenates in numeric order, strips per-file shebangs)
./build.sh

# Go is managed by mise (.mise.toml); run `mise install` once, then:
# Build the integration test binary (run from repo root)
GOOS=linux GOARCH=amd64 go build -o cmd/dockyardtest/dockyardtest_linux ./cmd/dockyardtest/
GOOS=linux GOARCH=arm64 go build -o cmd/dockyardtest/dockyardtest_linux_arm64 ./cmd/dockyardtest/
GOOS=darwin GOARCH=arm64 go build -o cmd/dockyardtest/dockyardtest_mac ./cmd/dockyardtest/
```

The `dockyard.sh` at the repo root is the working copy (not a symlink). `dist/dockyard.sh` is the built output. Edit `src/*.sh` and run `./build.sh` — never edit `dist/dockyard.sh` directly.

## Running the Integration Tests

The test suite (`cmd/dockyardtest/main.go`) SSHes into a remote Linux VM and runs 43 end-to-end tests across 3 dockyard instances. The only flags are `--host`, `--port`, `--user`, `--key`, `--timeout`, and `--only` (values: `""` for full suite, `btrfs` for BTRFS-only). There is no name-based filter.

```bash
# Run against a test VM (uploads dist/dockyard.sh automatically)
./dockyardtest_linux --host HOST --user USER [--key /path/to/key] [--port PORT]

# Sandman incus dockyard-test VM (via Tailscale, DNAT 2223 → VM:22)
./cmd/dockyardtest/dockyardtest_linux --host 100.100.218.64 --user thies --port 2223

# BTRFS-only subset
./dockyardtest_linux --host HOST --user USER --only btrfs
```

### Test VM Setup (sandman, incus)

The `dockyard-test` VM on sandman (100.100.218.64) runs the full 43-test suite. This is the primary test target; the old sandcastle `iso-test` VM at 100.106.185.92 is not maintained.

One-shot setup from your dev machine (everything after the `ssh` runs on sandman):

```bash
# 1. Cloud-init user-data: create `thies` with your pubkey, preinstall prereqs
MY_KEY=$(ssh-add -L | head -1)
ssh 100.100.218.64 "cat > /tmp/dockyard-cloud-init.yaml" <<EOF
#cloud-config
users:
  - name: thies
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    groups: [sudo]
    shell: /bin/bash
    ssh_authorized_keys:
      - ${MY_KEY}
ssh_pwauth: false
package_update: true
packages:
  - openssh-server
  - iptables
  - rsync
  - fuse3
  - iproute2
  - jq
  - curl
  - btrfs-progs
EOF

# 2. Launch the VM (uses incusbr0, DHCP)
ssh 100.100.218.64 'incus launch images:ubuntu/noble/cloud dockyard-test --vm \
    -c limits.cpu=4 -c limits.memory=8GiB \
    -d root,size=50GiB \
    -c user.user-data="$(cat /tmp/dockyard-cloud-init.yaml)"'

# 3. Wait for cloud-init to finish (ssh + apt install takes ~3–5 min on first run)
ssh 100.100.218.64 'incus exec dockyard-test -- cloud-init status --wait'

# 4. Unlock the `thies` account — cloud-init locks passwordless accounts by
#    default, and sshd refuses them even for pubkey auth. Set any password;
#    sshd then accepts pubkey logins.
ssh 100.100.218.64 'incus exec dockyard-test -- bash -c \
  "echo thies:\$(openssl rand -base64 20) | chpasswd"'

# 5. Find the VM IP and install host DNAT rules for SSH.
#    Incus proxy devices can not listen on 0.0.0.0 for VMs, so we NAT manually.
VM_IP=$(ssh 100.100.218.64 "incus list dockyard-test -c 4 --format csv" | awk '{print $1}')
ssh 100.100.218.64 "sudo iptables -t nat -C PREROUTING -p tcp --dport 2223 -j DNAT --to-destination ${VM_IP}:22 2>/dev/null || \
  sudo iptables -t nat -I PREROUTING -p tcp --dport 2223 -j DNAT --to-destination ${VM_IP}:22
  sudo iptables -C FORWARD -p tcp -d ${VM_IP} --dport 22 -j ACCEPT 2>/dev/null || \
  sudo iptables -I FORWARD -p tcp -d ${VM_IP} --dport 22 -j ACCEPT"

# 6. Verify SSH
ssh -p 2223 thies@100.100.218.64 'uname -a; which iptables rsync jq curl btrfs fusermount3'
```

### Running the tests

```bash
# Build the Linux test binary (needs mise-managed Go 1.26)
mise install
GOOS=linux GOARCH=amd64 go build -o cmd/dockyardtest/dockyardtest_linux ./cmd/dockyardtest/

# Build dockyard.sh
./build.sh

# Full suite (~20 min)
./cmd/dockyardtest/dockyardtest_linux --host 100.100.218.64 --user thies --port 2223
```

### Resetting the VM between runs

The suite cleans up after itself, but if it crashes mid-run:

```bash
ssh -p 2223 thies@100.100.218.64 '
  for p in dy1_ dy2_ dy3_; do
    sudo systemctl stop ${p}docker 2>/dev/null
    sudo rm -f /etc/systemd/system/${p}docker.service
    sudo rm -rf /${p%_} 2>/dev/null
    sudo userdel ${p}docker 2>/dev/null
  done
  sudo systemctl daemon-reload
'
```

### DNS scenario notes

`dockyard-test` uses `incusbr0`'s dnsmasq as its upstream DNS (10.146.154.1) and `/etc/resolv.conf` points at 127.0.0.53 (systemd-resolved). This reproduces the host shape described in issue #19, so test 10 (`daemon.json embeds host upstream DNS`) exercises the real `detect_upstream_dns()` path.

### Why DNAT instead of `incus proxy`

Incus proxy devices on VM instances require NAT mode, and NAT mode refuses `listen=tcp:0.0.0.0` *and* requires the target VM to have a statically-configured IP. The VM here uses DHCP, so we bypass the incus proxy entirely and install a manual iptables DNAT rule on sandman. The rule persists across VM reboots; it only needs re-adding if sandman itself reboots or the VM's DHCP lease hands out a different IP.

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
