# Findings

## Sysbox 0.6.7 CE: No configurable socket paths

**Date**: 2026-02-23
**Severity**: Architectural blocker for naive per-instance sysbox
**Status**: RESOLVED — forked sysbox (0.6.7.9-tc)

`sysbox-fs` and `sysbox-mgr` have NO flag to change their socket paths:
- `/run/sysbox/sysfs.sock` — hardcoded in sysbox-fs
- `/run/sysbox/sysmgr.sock` — hardcoded in sysbox-mgr

`sysbox-runc` has `--no-sysbox-fs` / `--no-sysbox-mgr` for debug only; no custom socket path flags.

**Confirmed via**: `sysbox-fs --help` and `sysbox-mgr --help` on target VM (Ubuntu 24.04.1).

### Consequence

Multiple dockyard instances **cannot** each run their own isolated sysbox daemon — the second instance's sysbox-fs will fail to bind the already-held socket. Under `Restart=on-failure` the service keeps restarting, appearing "active" while actually broken.

When the **first** instance (which successfully bound the socket) is destroyed, the socket disappears and all other instances immediately lose sysbox → DinD breaks.

### Intermediate solution (superseded): Shared sysbox daemon with ref-counting

An intermediate architecture used a single shared `dockyard-sysbox.service` per host (`Requires=` from each docker service). This was workable but meant all instances shared one sysbox process — no true per-instance isolation of the runtime daemon.

### Final solution: Fork sysbox to add `--run-dir`

The fork (`github.com/thieso2/sysbox`, version `0.6.7.9-tc`) adds `--run-dir <dir>` to all three sysbox binaries — `sysbox-mgr`, `sysbox-fs`, and `sysbox-runc`. Each dockyard instance now starts its own sysbox-mgr and sysbox-fs with a unique `--run-dir` pointing to `${DOCKYARD_ROOT}/run/sysbox/`. There is no shared sysbox service. `--run-dir` is passed via `runtimeArgs` in `daemon.json`; no wrapper script is needed.

---

## sysbox-runc has no --run-dir flag; reads SYSBOX_RUN_DIR env var instead

**Date**: 2026-02-24
**Severity**: Integration blocker for per-instance sysbox-runc
**Status**: RESOLVED in 0.6.7.9-tc — `runtimeArgs` now works; wrapper script no longer needed

`sysbox-runc` does not accept `--run-dir` as a CLI flag. Passing it via `runtimeArgs` in daemon.json causes exit status 1 at container start time.

**Root cause**: sysbox-runc reads its socket paths from the environment variable `SYSBOX_RUN_DIR`, not from a CLI argument. The variable is consumed in `libsysbox/sysbox/sysbox.go` `init()`, which calls `SetSockAddr()` on both the sysbox-mgr and sysbox-fs gRPC clients.

**Fix**: Install the real sysbox-runc binary as `sysbox-runc-bin`. Install a thin wrapper script at `sysbox-runc` that sets `SYSBOX_RUN_DIR` before exec-ing the real binary:

```sh
#!/bin/sh
export SYSBOX_RUN_DIR="/dy1/run/sysbox"
exec "/dy1/bin/sysbox-runc-bin" "$@"
```

daemon.json's `runtimes` block points to the wrapper with no `runtimeArgs`.

---

## build.sh grep -v '#!' stripped heredoc shebangs

**Date**: 2026-02-24
**Severity**: Build correctness bug — wrapper script shebangs silently removed
**Status**: RESOLVED — replaced with `awk 'NR==1 && /^#!/ {next} {print}'`

`build.sh` used `grep -v '^#!'` to strip the per-file shebang line before concatenating source files. This also stripped any `#!/bin/sh` line that appeared inside a heredoc in a source file (specifically the sysbox-runc wrapper script heredoc in `src/11_create.sh`).

**Symptom**: The installed `sysbox-runc` wrapper lacked its `#!/bin/sh` shebang and failed to execute.

**Fix**: Replace `grep -v '^#!'` with `awk 'NR==1 && /^#!/ {next} {print}'`, which skips only the first line of each source file when it is a shebang, leaving all subsequent lines intact regardless of their content.

---

## docker:dind latest (27.x) incompatible with sysbox 0.6.7

**Date**: 2026-02-23
**Severity**: DinD test failures (tests 10, 11, 12)

Error:
```
runc create failed: unable to start container process: error during container init:
open sysctl net.ipv4.ip_unprivileged_port_start file: unsafe procfs detected:
openat2 /proc/./sys/net/ipv4/ip_unprivileged_port_start: invalid cross-device link
```

**Root cause**: runc 1.3.x (used by Docker 27.x) added a strict "safe procfs" check that detects sysbox-fs's bind-mount of `/proc/sys` entries (which creates cross-device links). This was tracked in `opencontainers/runc#4968` and `nestybox/sysbox#973`.

**Affected**: inner containers launched by the `docker:dind` container's inner dockerd.

**Fix**: Pin `docker:dind` image to `docker:26.1-dind` (uses runc 1.1.12, before the 1.3.x series). runc 1.1.x does NOT have the cross-device link check.

| docker:dind tag | runc version | sysbox compat |
|----------------|-------------|---------------|
| `docker:dind` (latest = 27.x) | 1.3.3 | ❌ broken |
| `docker:26.1-dind` | 1.1.12 | ✅ works |
| `docker:25.0-dind` | 1.1.12 | ✅ works |

---

## Multi-instance bridge isolation: not implemented by default

**Date**: 2026-02-23
**Severity**: Test expectation mismatch (test was wrong, not a bug)

Containers in instance A can reach instance B's bridge IP. This is expected Linux behaviour — the kernel routes between all bridge interfaces on the same host. No `FORWARD DROP` rules exist between dockyard bridges.

**Verdict**: Not a bug. Dockyard's isolation is at the **daemon/socket/data** level, not at the network level. The isolation test was incorrectly expecting network-level isolation. Replaced with daemon-level isolation test (containers from A not visible in B's `docker ps`).

---

## sysbox-runc --run-dir CLI flag: seccomp socket not redirected (0.6.7.4–0.6.7.8-tc)

**Date**: 2026-02-24 (confirmed still broken through 0.6.7.8-tc; fixed in 0.6.7.9-tc: 2026-02-25)
**Severity**: Containers fail to start when `--run-dir` is used via `runtimeArgs`
**Status**: RESOLVED in 0.6.7.9-tc — tracked in https://github.com/thieso2/sysbox/issues/5

### Symptom

When `daemon.json` passes `--run-dir` via `runtimeArgs`, containers fail with:

```
container_linux.go:2573: sending seccomp fd to sysbox-fs caused:
Unable to establish connection with seccomp-tracer:
dial unix /run/sysbox/sysfs-seccomp.sock: connect: no such file or directory
```

This error occurs even though `sysfs-seccomp.sock` **is** correctly created at the
per-instance run-dir (e.g. `/dy1/run/sysbox/sysfs-seccomp.sock`). sysbox-runc ignores
the relocated socket and still dials the hardcoded `/run/sysbox/sysfs-seccomp.sock`.

Confirmed present in 0.6.7.4-tc through 0.6.7.7-tc. The `SYSBOX_RUN_DIR` env var path
(read directly in `init()`) works correctly in all versions.

### Confirmed call path via strace + binary wrapper

Instrumented with a debug wrapper at the sysbox-runc binary path. Confirmed:

1. containerd calls `sysbox-runc` directly (not via Docker-generated shim) with:
   `--run-dir /dy1/run/sysbox ... create --bundle ...`
2. `SYSBOX_RUN_DIR` is **not set** in containerd's environment
3. `sysbox.go init()` runs → reads unset `SYSBOX_RUN_DIR` → `runDir = "/run/sysbox"`
4. `app.Before()` calls `sysbox.SetRunDir(context.GlobalString("run-dir"))`
5. Despite `--run-dir /dy1/run/sysbox` in argv, `context.GlobalString("run-dir")` returns
   the default `/run/sysbox` — urfave/cli v1 bug with global flags before subcommands
6. `SetRunDir("/run/sysbox")` is a no-op (same as default)
7. `SendSeccompInit` dials `/run/sysbox/sysfs-seccomp.sock` → fails

Adding `export SYSBOX_RUN_DIR=/dy1/run/sysbox` to the wrapper env makes it work instantly —
confirming the env var path through `init()` is the only reliable mechanism.

### Root cause: urfave/cli v1 GlobalString in app.Before

`context.GlobalString("run-dir")` in `app.Before` does not return the CLI-provided value
`/dy1/run/sysbox`. It returns the flag default `/run/sysbox`. This is a known urfave/cli v1
quirk where global flags passed before a subcommand may not be visible to `app.Before`'s
root context via `GlobalString`.

The 0.6.7.7-tc fix (`os.Setenv("SYSBOX_RUN_DIR", dir)` in `SetRunDir`) does not help
because `SetRunDir` is called with the wrong value (the default).

`SYSBOX_RUN_DIR` env var works correctly because `sysbox.go`'s `init()` reads it before
`app.Before` runs — so `runDir` is set to the correct per-instance path from the start.

### Workaround (used in dockyard 0.6.7.4-tc through 0.6.7.8-tc)

Wrapper script that exports `SYSBOX_RUN_DIR` before exec'ing the real binary:

```sh
#!/bin/sh
export SYSBOX_RUN_DIR="/dy1/run/sysbox"
exec "/dy1/bin/sysbox-runc-bin" "$@"
```

daemon.json pointed to the wrapper with no `runtimeArgs`. No longer needed in 0.6.7.9-tc.

### Fix (0.6.7.9-tc)

Extended `init()` in `sysbox.go` to scan `os.Args` directly for `--run-dir` before urfave/cli
runs. This bypasses `context.GlobalString` entirely and makes `runtimeArgs` work correctly.
The wrapper script and `sysbox-runc-bin` alias are no longer needed.

See: https://github.com/thieso2/sysbox/issues/5

---

## sysbox 0.6.7 incompatible with Linux kernel 6.17 (Ubuntu 25.10+)

**Date**: 2026-02-25
**Severity**: Hard blocker — containers fail to start; no workaround without patching sysbox-runc
**Status**: OPEN — sysbox 0.6.7.x not fixed; Ubuntu 24.04 LTS (kernel 6.8) confirmed working

### Symptom

`docker run` exits immediately with:

```
docker: Error response from daemon: failed to create task for container:
failed to create shim task: OCI runtime create failed:
runc create failed: ... EOF
```

The error appears generic. The OCI runtime log is deleted by containerd before it can be read directly.

### Diagnosis

Install a debug wrapper at the sysbox-runc binary path that copies `--log` output before exec:

```sh
#!/bin/sh
LOGFILE="/tmp/sysbox-runc-$$.json"
exec /path/to/sysbox-runc-real --log "$LOGFILE" "$@"
```

The captured `log.json` shows the fatal entry:

```
nsexec:1050 nsenter: failed to set rootfs parent mount propagation to private: Permission denied
```

### Root cause

`nsexec.c` in sysbox-runc (the C preamble that runs before the Go runtime) calls:

```c
mount("", "/", "", MS_PRIVATE | MS_REC, "")
```

This attempts to change the root mount's propagation to private from within a new user+mount namespace. On **kernel 6.17** (Ubuntu 25.10+), inherited mounts owned by the parent user namespace cannot have their propagation changed from a child user namespace — the kernel returns `EPERM`.

This restriction was tightened in a patch carried by Ubuntu's 6.17 kernel. **Mainline 6.17, 6.18, 6.16 are all confirmed working** — the break is specific to Ubuntu 25.10's `6.17.0-14-generic` build. Mainline `runc` ≥ 1.2 handles the `EPERM` gracefully regardless; sysbox-runc 0.6.7.x does not.

**Confirmed not fixable by**:
- `--privileged` — nsexec runs before privilege escalation is meaningful here
- `--security-opt seccomp=unconfined` — not a seccomp issue
- AppArmor changes — not an AppArmor issue
- `SYSBOX_RUN_DIR` / `--run-dir` flags — unrelated to namespace setup

### Affected environments

| Kernel build | Status |
|---|---|
| Ubuntu 25.10 `6.17.0-14-generic` | ❌ broken |
| mainline `6.17.0-061700-generic` | ✅ confirmed working |
| mainline `6.18.0-061800-generic` | ✅ confirmed working |
| mainline `6.16.0-061600-generic` | ✅ confirmed working |
| Ubuntu 25.04 `6.14.0-37-generic` | ✅ confirmed working |
| Ubuntu 24.04 LTS `6.8.x-generic` | ✅ confirmed working |
| Ubuntu 22.04 LTS `5.15.x-generic` | ✅ expected working |

The Ubuntu 25.10 kernel carries an Ubuntu-specific patch that tightens user-namespace mount propagation rules. The same kernel version from the mainline archive does not have this restriction.

### Fix

Two options:
1. **Replace kernel**: Install mainline 6.17 or 6.18 (`kernel.ubuntu.com/mainline`) instead of Ubuntu's 6.17 build.
2. **Patch sysbox-runc**: Update `nsexec.c` to handle `EPERM` on the `mount --make-private` call gracefully. No such patch is available in 0.6.7.x as of 2026-02-26.

---

## Test cleanup check false negative

**Date**: 2026-02-23

The cleanup test checked `ip link show dy2_docker0` and parsed the output for "does not exist" string. When the bridge is gone, the command exits non-zero AND prints "Device ... does not exist." — the logic was correct, but the bridge may have been left by the pool cleanup bug. Resolved by fixing the underlying destroy and using exit-code-based checks.

---

## verify: exact-match checks fail when alpine image not cached

**Date**: 2026-02-26
**Severity**: Test false-positive — `verify` reported FAIL on working instances
**Status**: RESOLVED — `src/18_verify.sh` (two separate fixes)

Both the basic container run check (check 4) and the DinD inner container check (check 6) used exact-string match against the expected output:

```bash
# check 4
out=$(... docker run --rm alpine echo verify-ok 2>&1)
if [ "$out" = "verify-ok" ]; then

# check 6
out=$(... docker exec "$cname" docker run --rm alpine echo dind-ok 2>&1)
if [ "$out" = "dind-ok" ]; then
```

When the alpine image is not cached — either in the outer daemon (check 4) or inside the DinD container (check 6) — docker pull progress lines appear in stdout alongside the expected string. The exact-string match fails even though the container ran correctly.

The DinD check was fixed first (noticed on 100.106.185.92 where the image was cold). The basic container run check was caught later when running on sandman with a freshly created instance.

**Fix**: Replace both exact matches with `echo "$out" | grep -q "..."`:

```bash
if echo "$out" | grep -q "verify-ok"; then
if echo "$out" | grep -q "dind-ok"; then
```

---

## Reliability audit: 8 issues found and fixed

**Date**: 2026-02-26
**Severity**: Mix of critical, high, and medium
**Status**: RESOLVED — commit `2c87943`

A structured review identified 8 bugs across the service lifecycle. All fixed in a single commit; all 29 dockyardtest tests pass after.

### 1. Stale sysbox sockets fool the wait loop (Critical)

**Symptom**: After an unclean shutdown, `sysmgr.sock` / `sysfs.sock` / `sysfs-seccomp.sock` persist on disk. The socket wait loop checked `[ ! -e file ]` (any file type), so the stale socket immediately satisfied the check. The service appeared to start but the first `docker run` failed with a gRPC connection error to sysbox.

**Fix** (`src/12_enable.sh`, `src/14_start.sh`): Added `ExecStartPre` that removes the three sysbox socket files before starting sysbox-mgr and sysbox-fs. Also applied in `cmd_start`.

### 2. Socket wait loop used `-e` instead of `-S` (Critical)

**Symptom**: `wait_for_file` and all inline wait loops in the service file used `[ ! -e "$file" ]` — satisfied by any file at the path, not just a Unix domain socket. A directory, regular file, or broken symlink at the socket path would satisfy the check.

**Fix** (`src/02_helpers.sh`, `src/12_enable.sh`): Changed all wait conditions to `[ ! -S "$file" ]` (socket type check).

### 3. No alive check in socket wait loops (High)

**Symptom**: If a daemon crashed after creating its socket but before finishing initialization, the wait loop returned immediately (socket file exists). If a daemon crashed before creating the socket, the loop waited the full 30 s timeout. Neither case detected the crash promptly.

**Fix** (`src/12_enable.sh`, `src/14_start.sh`): Added `kill -0 $PID` inside every wait loop — if the process is no longer alive the loop exits with an error immediately, avoiding both the silent-success and the long-timeout failure modes.

### 4. iptables duplicate rules on service restart (Critical)

**Symptom**: The service file used bare `iptables -I` without checking whether a rule already existed. On `Restart=on-failure`, rules were inserted a second time at the head of the FORWARD chain, producing duplicates. `ExecStopPost` uses `-D` to remove rules, but if cleanup was incomplete (e.g., a partially failed start), the restart inserted additional copies.

**Fix** (`src/12_enable.sh`): Replaced every `iptables -I` in `ExecStartPre` with the idempotent `iptables -C ... 2>/dev/null || iptables -I ...` pattern already used in `cmd_start`.

### 5. Concurrent create: download() TOCTOU / partial-tarball corruption (High)

**Symptom**: Two concurrent `create` invocations both passed the `[ -f "$dest" ]` check (false) and started `curl -o "$dest" "$url"` simultaneously, each writing to the same destination file. One curl's partial write could be read by the other instance's `tar` extraction, producing a corrupted tarball.

**Fix** (`src/11_create.sh`): Changed `curl -o "$dest"` to `curl -o "${dest}.tmp" && mv "${dest}.tmp" "$dest"`. The `mv` (rename syscall) is atomic; the worst case is two complete downloads where the second overwrites the first with an identical file.

### 6. Staging directory leaked on Ctrl-C (Medium)

**Symptom**: `cmd_create` used `trap 'rm -rf "$STAGING"' RETURN` to clean up the per-PID staging directory. RETURN fires when the function returns normally, but not on SIGINT or SIGTERM. A Ctrl-C during download/extraction left an orphaned `staging-$$` directory under `.tmp/`.

**Fix** (`src/11_create.sh`): Extended to `trap 'rm -rf "$STAGING"' RETURN EXIT INT TERM`.

### 7. Concurrent AppArmor write/remove TOCTOU (Medium)

**Symptom**: `cmd_create` appended to `/etc/apparmor.d/local/fusermount3` after a `grep` check — two concurrent creates for different instances could both pass the check and both append, resulting in duplicate blocks. `cmd_destroy` used `awk > .tmp && mv` without any locking — two concurrent destroys could each read the same file, each write their own `.tmp`, and the second `mv` would silently discard the first's removal.

**Fix** (`src/11_create.sh`, `src/17_destroy.sh`): Wrapped both operations in `flock -x 9` on a `.lock` file alongside the AppArmor file. The check-then-write and the awk-then-rename are now serialized.

### 8. `verify` DinD output check used exact match (Medium)

See separate entry above. Same root cause: docker pull output mixed into stdout.

---

## staging trap EXIT fires after cmd_create returns — unbound variable

**Date**: 2026-02-26
**Severity**: Create fails on every run (regression from reliability audit fix #6)
**Status**: RESOLVED — commit `089c3c8`

### Symptom

`dockyard.sh create` exited immediately after enabling the systemd service with:

```
/home/thies/dockyard.sh: line 1: STAGING: unbound variable
```

### Root cause

The reliability audit added `EXIT` to the staging-directory cleanup trap:

```bash
trap 'rm -rf "$STAGING"' RETURN EXIT INT TERM
```

`trap ... EXIT` sets the **script-level** EXIT handler, not a function-level one. It fires when the entire script process exits — which happens *after* `cmd_create` has already returned and its `local STAGING` variable has gone out of scope. With `set -u`, referencing an unbound variable is a fatal error, so the script died at script exit rather than after `create` completed.

`RETURN` already fires when the function returns normally (the original intent). `INT` and `TERM` fire while still inside the function, where `STAGING` is in scope.

### Fix

Drop `EXIT` from the trap; keep `RETURN INT TERM`:

```bash
trap 'rm -rf "$STAGING"' RETURN INT TERM
```
