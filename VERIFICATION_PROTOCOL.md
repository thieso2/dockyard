# Dockyard Verification Protocol

Comprehensive verification checklist for dockyard releases. Covers standalone dockyard testing (multi-instance, lifecycle, isolation, reboot persistence) and downstream Sandcastle integration.

## Prerequisites

| Requirement | Details |
|-------------|---------|
| Test host | Linux VM with Ubuntu 24.04+ (bare metal or Incus VM, NOT a container) |
| Resources | 4+ CPUs, 4+ GB RAM, 20+ GB disk |
| Networking | Static IPv4, working DNS, outbound internet access |
| Kernel | Test on at least one stock kernel and one HWE/mainline kernel |
| SSH | Passwordless SSH access for the test runner |

---

## 1. Build Verification

- [ ] `./build.sh` produces `dist/dockyard.sh` without errors
- [ ] Built script starts with `#!/bin/bash` and `set -euo pipefail`
- [ ] No duplicate shebangs (per-file shebangs stripped during build)
- [ ] `dist/dockyard.sh` is executable
- [ ] Go test binary builds for linux/amd64 and linux/arm64

## 2. gen-env

### Basic generation
- [ ] `./dockyard.sh gen-env` creates `dockyard.env` with all 6 variables
- [ ] Generated bridge CIDR is in RFC 1918 range (172.16-31.x.x/24)
- [ ] Generated pool base is in RFC 1918 range (172.16-31.x.x/16)
- [ ] Bridge and pool use different /16 second octets (no overlap)
- [ ] Fixed CIDR matches bridge CIDR network (same /24)
- [ ] Default `DOCKYARD_ROOT=/dockyard`, `DOCKYARD_DOCKER_PREFIX=dy_`, `DOCKYARD_POOL_SIZE=24`

### Override support
- [ ] `DOCKYARD_ROOT=/custom` overrides root directory
- [ ] `DOCKYARD_DOCKER_PREFIX=test_` overrides prefix
- [ ] `DOCKYARD_BRIDGE_CIDR`, `DOCKYARD_FIXED_CIDR`, `DOCKYARD_POOL_BASE` accept explicit values
- [ ] `DOCKYARD_ENV=/path/to/file` writes to custom path instead of `./dockyard.env`

### Conflict detection
- [ ] Errors if output file already exists
- [ ] Errors if bridge name already exists (`check_prefix_conflict`)
- [ ] Errors if systemd service already exists (`check_prefix_conflict`)
- [ ] Errors if `${root}/bin/` already exists (`check_root_conflict`)
- [ ] Errors if CIDRs overlap existing routes (`check_subnet_conflict`)
- [ ] Errors if CIDRs overlap sibling `.env` files in same directory (`check_sibling_conflict`)
- [ ] Errors if any CIDR is outside RFC 1918 ranges (`check_private_cidr`)
- [ ] `--nocheck` bypasses all conflict checks
- [ ] Retries up to 10 times on collision when randomizing

### Multi-instance gen-env
- [ ] 3 instances can be generated side-by-side with different prefixes/roots
- [ ] No network overlap between any pair of generated configs

## 3. create

### Preflight checks
- [ ] Errors if `curl` is missing
- [ ] Errors if `iptables` is missing
- [ ] Errors if `rsync` is missing
- [ ] Error message lists all missing tools at once

### Architecture detection
- [ ] Detects `x86_64` and downloads correct binaries
- [ ] Detects `aarch64` and downloads correct binaries
- [ ] Errors on unsupported architectures

### Binary downloads
- [ ] Docker CE static binaries downloaded and SHA256-verified
- [ ] Docker Rootless Extras downloaded and SHA256-verified
- [ ] Sysbox static tarball downloaded and SHA256-verified
- [ ] Docker Compose plugin downloaded and SHA256-verified
- [ ] Cached downloads are reused on subsequent creates
- [ ] Cached downloads are also SHA256-verified (cache poisoning protection)
- [ ] Download uses atomic temp files (`${dest}.tmp.$$`) to prevent concurrent create races
- [ ] SHA256 mismatch deletes the corrupt file and exits with error

### Binary installation
- [ ] All Docker binaries installed to `${DOCKYARD_ROOT}/bin/`
- [ ] All 3 sysbox binaries installed: `sysbox-runc`, `sysbox-mgr`, `sysbox-fs`
- [ ] Docker CLI renamed to `docker-cli`, replaced by wrapper script
- [ ] Wrapper script bakes in `DOCKER_HOST` and `DOCKER_CONFIG`
- [ ] Docker Compose installed to `${DOCKER_CONFIG_DIR}/cli-plugins/docker-compose`
- [ ] `${BIN_DIR}/docker compose version` works and shows correct version
- [ ] `dockyard.sh` and `dockyardctl` symlink installed to `${BIN_DIR}/`
- [ ] `dockyard.env` copied to `${ETC_DIR}/`

### daemon.json
- [ ] `default-runtime` is `sysbox-runc`
- [ ] `runtimeArgs` includes `--run-dir ${SYSBOX_RUN_DIR}`
- [ ] `storage-driver` is `overlay2`
- [ ] `userland-proxy-path` points to `${BIN_DIR}/docker-proxy`
- [ ] `buildkit` enabled

### System user/group
- [ ] System group `${PREFIX}docker` created
- [ ] System user `${PREFIX}docker` created (no home, `/bin/false` shell)
- [ ] `${DOCKYARD_ROOT}` owned by `${INSTANCE_USER}:${INSTANCE_GROUP}`

### AppArmor (Ubuntu 25.10+)
- [ ] Tagged block added to `/etc/apparmor.d/local/fusermount3`
- [ ] Block includes `capability dac_override`
- [ ] Block includes FUSE mount rule for `${SYSBOX_DATA_DIR}`
- [ ] `apparmor_parser -r` reloads the fusermount3 profile
- [ ] Multiple instances add separate tagged blocks (not overwritten)

### Systemd service (default behavior)
- [ ] `${PREFIX}docker.service` written to `/etc/systemd/system/`
- [ ] Service enabled via `systemctl enable`
- [ ] Service started via `systemctl start`
- [ ] Service type is `notify` with `NotifyAccess=all`

### Options
- [ ] `--no-systemd` skips service install and enable
- [ ] `--no-start` skips starting the daemon
- [ ] `--no-systemd --no-start` installs binaries only

## 4. enable (Systemd Service)

### dockyard-stack script
- [ ] Written to `${BIN_DIR}/dockyard-stack`
- [ ] Starts sysbox-mgr, sysbox-fs, containerd, dockerd in order
- [ ] Waits for each socket with 60-second timeout
- [ ] Sends `systemd-notify --ready` after all sockets are up
- [ ] Monitor loop detects daemon death and triggers cleanup
- [ ] TERM/INT signals trigger orderly shutdown (reverse order)

### Service unit
- [ ] ExecStartPre creates required directories
- [ ] ExecStartPre cleans stale sockets (docker, containerd, sysbox)
- [ ] ExecStartPre enables IP forwarding (`net.ipv4.ip_forward=1`)
- [ ] ExecStartPre creates bridge with correct CIDR
- [ ] ExecStartPre inserts bridge iptables rules (FORWARD + NAT MASQUERADE)
- [ ] ExecStartPre inserts pool iptables rules (FORWARD + NAT MASQUERADE)
- [ ] ExecStartPost waits for Docker API readiness (360 half-second polls)
- [ ] ExecStopPost removes iptables rules (bridge + pool)
- [ ] ExecStopPost removes bridge
- [ ] ExecStopPost cleans up sysbox run dir
- [ ] ExecStopPost cleans up docker/containerd sockets
- [ ] `Restart=on-failure` with `RestartSec=5`
- [ ] `KillMode=process` (only kills main process, not child daemons)
- [ ] All rules are idempotent (`-C` check before `-I` insert)

## 5. start (Non-Systemd)

- [ ] Starts sysbox-mgr with `--run-dir` and `--data-root`
- [ ] Waits for `sysmgr.sock` (60-second timeout)
- [ ] Starts sysbox-fs with `--run-dir` and `--mountpoint`
- [ ] Waits for `sysfs.sock` (60-second timeout)
- [ ] Creates bridge if not exists
- [ ] Enables IP forwarding
- [ ] Inserts iptables rules (idempotent)
- [ ] Starts containerd with instance-specific root/state/address
- [ ] Starts dockerd with all instance-specific flags
- [ ] Cleanup function kills started daemons on failure

## 6. stop

- [ ] Stops daemons in reverse order: dockerd, containerd, sysbox-fs, sysbox-mgr
- [ ] Removes docker and containerd sockets
- [ ] Removes bridge iptables rules
- [ ] Removes pool iptables rules
- [ ] Removes bridge interface
- [ ] Cleans up leftover user-defined network bridges from pool range

## 7. status

- [ ] Displays all 6 config variables
- [ ] Displays all derived variables (RUN_DIR, BRIDGE, SERVICE_NAME, sockets)
- [ ] Reports systemd service state (active/inactive/not installed)
- [ ] Reports PID status for sysbox-mgr, sysbox-fs, containerd, dockerd
- [ ] Reports bridge state and IP
- [ ] Reports socket existence
- [ ] Runs connectivity test (ping from container)
- [ ] Works without root (uses `/proc/$pid` instead of `kill -0`)

## 8. verify

- [ ] Check 1: systemd service active
- [ ] Check 2: docker socket exists
- [ ] Check 3: docker info (API reachable)
- [ ] Check 4: container run (alpine echo)
- [ ] Check 5: outbound networking (ping 1.1.1.1 from container)
- [ ] Check 6: Docker-in-Docker (docker:26.1-dind start + inner container run)
- [ ] Exits 0 only when all 6 checks pass
- [ ] Exits non-zero with count of failures
- [ ] DinD waits up to 60 seconds for inner dockerd
- [ ] Preloads alpine from `/var/tmp/alpine.tar` if available (rate limit avoidance)

## 9. disable

- [ ] Stops service if active
- [ ] Disables service
- [ ] Removes service file
- [ ] Runs `systemctl daemon-reload`
- [ ] Warns if service file doesn't exist (no error)

## 10. destroy

### Standard destroy
- [ ] Stops and removes systemd service
- [ ] Removes AppArmor fusermount3 block for this instance
- [ ] Removes `${DOCKYARD_ROOT}` entirely
- [ ] Removes system user `${PREFIX}docker`
- [ ] Removes system group `${PREFIX}docker`
- [ ] Cleans up leftover user-defined network bridges from pool range
- [ ] Prompts for confirmation (unless `--yes`)

### `--keep-data` mode
- [ ] Removes bin/, etc/, log/, run/, lib/sysbox, lib/docker-config
- [ ] Preserves `${DOCKER_DATA}` (images, containers, volumes)
- [ ] Shows preserved data path

### Edge cases
- [ ] Destroy with running containers completes (containers killed by daemon stop)
- [ ] Double destroy is idempotent (second call is a no-op, exit 0)
- [ ] Destroy without systemd service falls back to direct PID-based stop
- [ ] Service file gone after destroy
- [ ] Bridge gone after destroy
- [ ] No residual iptables rules after destroy
- [ ] Instance root directory gone after destroy
- [ ] Sysbox run directory gone after destroy

## 11. Environment Loading

- [ ] `DOCKYARD_ENV` variable: loads specified file, errors if missing
- [ ] `./dockyard.env` in current directory: auto-detected
- [ ] `../etc/dockyard.env` relative to script: works for installed copy
- [ ] `${DOCKYARD_ROOT}/etc/dockyard.env`: fallback path
- [ ] No config found: clear error message suggesting `gen-env` or `DOCKYARD_ENV`
- [ ] `gen-env` does NOT go through `load_env()`

---

## 12. Multi-Instance Tests (3 concurrent instances)

These tests verify that N dockyard instances operate independently on the same host. Run with the Go integration test binary (`dockyardtest`).

### Concurrent create
- [ ] 3 instances created concurrently with staggered start (3s apart)
- [ ] All 3 instances come up with services active
- [ ] Download cache shared without corruption (PID-suffixed temp files)

### Per-instance daemon isolation
- [ ] Each instance has its own sysbox-mgr, sysbox-fs, containerd, dockerd
- [ ] Containers in instance A are NOT visible in instance B's `docker ps`
- [ ] Containers in instance A are NOT visible in instance C's `docker ps`
- [ ] Containers in instance B are NOT visible in instance C's `docker ps`

### Per-instance networking
- [ ] Each instance has its own bridge (e.g., `dy1_docker0`, `dy2_docker0`)
- [ ] Each instance has its own iptables rules scoped to its bridge
- [ ] Outbound networking works from all instances simultaneously
- [ ] DNS resolution works from all instances simultaneously

### Per-instance Docker-in-Docker
- [ ] DinD containers start on all 3 instances concurrently (sysbox, no --privileged)
- [ ] Inner dockerd reaches "ready" state within 120 seconds on each
- [ ] Inner containers can be created on all instances
- [ ] Inner containers have outbound networking on all instances

### Per-instance socket permissions
- [ ] Socket not world-accessible (last permission octet is 0)
- [ ] Socket group is `${PREFIX}docker` (instance-specific group)

### Selective destroy with survivors
- [ ] Destroying instance A does not affect instance B or C
- [ ] B and C remain healthy (containers run, networking works) after A is destroyed
- [ ] A's service, bridge, and iptables rules are fully removed
- [ ] A's data directory is removed
- [ ] A's system user and group are removed

### Reboot persistence
- [ ] After host reboot, surviving instances (B+C) auto-start via systemd
- [ ] Services reach "active" state within 90 seconds of boot
- [ ] Container runs succeed on all surviving instances after reboot
- [ ] Outbound networking works after reboot
- [ ] DinD works after reboot (full: start + inner container + inner networking)
- [ ] Destroyed instance (A) does NOT come back after reboot

### Full cleanup
- [ ] After destroying all instances: no services, bridges, iptables rules, data dirs, or users remain

### Nested DOCKYARD_ROOT
- [ ] Deeply nested root (e.g., `/var/tmp/dockyard-nested/level1/level2/dockyard`) works
- [ ] Full lifecycle: gen-env, create, container run, destroy
- [ ] Directory fully cleaned up after destroy

## 13. Sysbox-Specific Tests

- [ ] Default runtime is `sysbox-runc` (verified via `docker info`)
- [ ] `--run-dir` correctly isolates per-instance sysbox sockets
- [ ] Three independent sysbox daemon sets run simultaneously without conflict
- [ ] FUSE mounts work for sysbox-fs (required for `/proc`, `/sys` emulation)
- [ ] Bind-mount mkdir inside sysbox containers works (tests kernel UID mapping)

## 14. Stop/Start Cycle

- [ ] `systemctl stop` stops the service cleanly
- [ ] Service reports inactive after stop
- [ ] `systemctl start` restarts successfully
- [ ] Containers can run after restart
- [ ] iptables rules are correctly re-inserted on start
- [ ] Bridge is recreated if needed

---

## 15. Sandcastle Integration

Tests that dockyard works correctly when deployed as part of the Sandcastle platform.

### Installer integration
- [ ] Sandcastle `installer.sh` bundles the correct dockyard version (via `@@TEMPLATE:templates/dockyard.sh@@`)
- [ ] `installer/build.sh` successfully rebuilds `installer.sh` after template update
- [ ] Installer creates `dockyard.env` with Sandcastle-specific values (DOCKYARD_ROOT, PREFIX, CIDRs)
- [ ] `DOCKYARD_ENV=$env bash /tmp/dockyard.sh create` succeeds within the installer flow
- [ ] Docker socket appears within 30 seconds of dockyard create

### Dockyard health inside Sandcastle
- [ ] All 4 daemons running: sysbox-mgr, sysbox-fs, containerd, dockerd
- [ ] `docker compose version` returns Compose v2.32.4
- [ ] `dockyard verify` passes all 6 checks
- [ ] Default runtime is `sysbox-runc`
- [ ] Bridge and pool iptables rules are in place

### Sandcastle services
- [ ] Sandcastle containers start: web, worker, postgres, traefik
- [ ] Postgres reaches healthy state
- [ ] Rails app boots (Puma listening)
- [ ] Traefik proxies requests to Rails
- [ ] Web health check (`/up`) returns HTTP 200 with green body
- [ ] Database migrations run successfully

### Sandbox lifecycle (end-to-end)
- [ ] Sandbox create: sysbox container starts with correct runtime
- [ ] Docker inside sandbox: inner Docker daemon is running
- [ ] DinD inside sandbox: inner container can be created and run
- [ ] Outbound networking from sandbox: ping to external IP works
- [ ] Sandbox stop: container stops cleanly, status updates to "stopped"
- [ ] Sandbox destroy: container removed, status updates to "destroyed", no leftover containers

### Network integration
- [ ] Dockyard pool base matches Sandcastle's DOCKYARD_POOL_BASE env var
- [ ] NAT MASQUERADE covers the entire pool /16
- [ ] sandcastle-web network is allocated from dockyard pool
- [ ] Containers on user-defined networks have internet access

---

## 16. Cross-Kernel Testing

Run the full test suite on multiple kernel versions to catch regressions:

| Kernel | Source | Priority |
|--------|--------|----------|
| 6.8.x | Ubuntu 24.04 stock | Required |
| 6.11.x | Ubuntu 24.04 HWE | Recommended |
| 6.14.x+ | Ubuntu HWE or mainline | Recommended |
| 6.17.x+ | Mainline | Best-effort |

Key things that vary across kernels:
- [ ] Sysbox user-namespace behavior (UID mapping, procfs access)
- [ ] FUSE mount permissions for sysbox-fs
- [ ] Bind-mount mkdir behavior with non-root-owned directories
- [ ] Overlayfs behavior for Docker storage driver

---

## 17. Architecture Testing

| Architecture | Platform | Priority |
|-------------|----------|----------|
| x86_64 | Ubuntu 24.04 VM | Required |
| aarch64 | Ubuntu 24.04 (ARM server/VM) | Required for releases |

Per-architecture verification:
- [ ] Correct binary URLs constructed
- [ ] Correct SHA256 checksums matched
- [ ] All binaries execute (no architecture mismatch errors)
- [ ] Docker Compose plugin works on target architecture
- [ ] Full test suite passes (at minimum: create, container run, DinD, destroy)

---

## Running the Automated Tests

The Go integration test binary covers tests 1-30 (sections 2-14 above) automatically.

```bash
# Build test binary (from repo root)
GOOS=linux GOARCH=amd64 go build -o cmd/dockyardtest/dockyardtest_linux ./cmd/dockyardtest/

# Run full suite against a target host
./dockyardtest_linux --host HOST --user USER [--key /path/to/key]

# Run specific test by name (substring match)
./dockyardtest_linux --host HOST --user USER --run "DinD"
./dockyardtest_linux --host HOST --user USER --run "verify"
```

### Expected test list (30 tests)

| # | Phase | Test |
|---|-------|------|
| 01 | Setup | Upload dockyard.sh |
| 02 | Setup | gen-env A |
| 03 | Setup | gen-env B |
| 04 | Setup | gen-env C |
| 05 | Create | create all instances (A+B+C concurrent) |
| 06 | Health | all instances: per-instance docker services active |
| 07 | Run | all instances: container run |
| 08 | Network | all instances: outbound ping |
| 09 | Network | all instances: DNS resolution |
| 10 | DinD | all instances: DinD start |
| 11 | DinD | all instances: DinD inner container |
| 12 | DinD | all instances: DinD inner networking |
| 13 | Isolation | multi-instance isolation (all pairs) |
| 14 | Verify | all instances: verify passes (6/6 checks) |
| 15 | Lifecycle | stop/start cycle |
| 16 | Security | socket permissions |
| 17 | Sysbox | sysbox bind-mount mkdir |
| 18 | Destroy | destroy under load |
| 19 | Destroy | double destroy idempotency |
| 20 | Destroy | A: fully cleaned up (service+bridge+iptables) |
| 21 | Survive | B+C: still healthy after A destroy |
| 22 | Reboot | reboot |
| 23 | Reboot | post-reboot: B+C services active |
| 24 | Reboot | post-reboot: B+C container run |
| 25 | Reboot | post-reboot: B+C outbound networking |
| 26 | Reboot | post-reboot: B+C DinD full |
| 27 | Cleanup | destroy B |
| 28 | Cleanup | destroy C |
| 29 | Cleanup | full cleanup verification |
| 30 | Edge | nested DOCKYARD_ROOT lifecycle |

### Sandcastle integration (manual)

Section 15 tests are run manually on an Incus VM:

```bash
# Create VM
incus launch ubuntu:24.04 sc-test --vm -c limits.cpu=4 -c limits.memory=4GiB -d root,size=30GiB -n incusbr0

# Configure static IP + DNS (remove default DHCP config first)
# Push installer.sh, gen-env, set admin password, run install
# Verify: containers, compose, dockyard verify, web health, sandbox lifecycle
```

---

## Release Checklist

Before tagging a release:

- [ ] `./build.sh` succeeds
- [ ] All 30 automated tests pass on x86_64 Ubuntu 24.04
- [ ] `dockyard verify` passes on a fresh install
- [ ] Sandcastle integration test passes (installer e2e + sandbox lifecycle)
- [ ] Bundled dockyard.sh in Sandcastle updated (`installer/templates/dockyard.sh`)
- [ ] Sandcastle `installer/build.sh` rebuilds cleanly
- [ ] CLAUDE.md updated if software versions changed
- [ ] Tag created and pushed
- [ ] GitHub release created with `dist/dockyard.sh` as release asset
