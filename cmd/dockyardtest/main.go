package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ── Flags ────────────────────────────────────────────────────────────────────

var (
	hostFlag    = flag.String("host", "", "Target host IP or hostname (required)")
	portFlag    = flag.Int("port", 22, "SSH port (default: 22)")
	userFlag    = flag.String("user", "", "SSH username (required)")
	keyFlag     = flag.String("key", "", "Path to SSH private key (default: ~/.ssh/id_ed25519)")
	timeoutFlag = flag.Duration("timeout", 20*time.Minute, "Overall test timeout")
	onlyFlag    = flag.String("only", "", "Run only a specific test group: btrfs, isolation, dind, etc.")
)

// ── Instance descriptor ───────────────────────────────────────────────────────

type Instance struct {
	Label   string // "A", "B", "C"
	Prefix  string // "dy1_"
	Root    string // "/dy1"
	EnvFile string // "~/dy1.env"
	Socket  string // "/dy1/run/docker.sock"
	Docker  string // "/dy1/bin/docker" (wrapper with DOCKER_HOST baked in)
}

var allInstances = []Instance{
	{"A", "dy1_", "/dy1", "~/dy1.env", "/dy1/run/docker.sock", "/dy1/bin/docker"},
	{"B", "dy2_", "/dy2", "~/dy2.env", "/dy2/run/docker.sock", "/dy2/bin/docker"},
	{"C", "dy3_", "/dy3", "~/dy3.env", "/dy3/run/docker.sock", "/dy3/bin/docker"},
}

// ── SSH helpers ───────────────────────────────────────────────────────────────

// dialSSH tries the SSH agent first (handles passphrase-protected keys
// transparently), then falls back to plain key files.
func dialSSH(host string, port int, user, keyPath string) (*ssh.Client, error) {
	var authMethods []ssh.AuthMethod

	// SSH agent — already holds the decrypted key, no passphrase needed.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			authMethods = append(authMethods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	// Key file fallback (unprotected keys only; passphrase ones are silently skipped).
	paths := []string{keyPath}
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		paths = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}
	for _, kp := range paths {
		data, err := os.ReadFile(kp)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue // passphrase-protected — agent path above handles it
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no auth methods available — SSH_AUTH_SOCK not set and no unprotected key found at %v", paths)
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         15 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return client, nil
}

// run executes cmd on the remote host; never errors on non-zero exit codes.
func run(client *ssh.Client, cmd string) (stdout, stderr string, exitCode int) {
	session, err := client.NewSession()
	if err != nil {
		return "", err.Error(), 1
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf

	err = session.Run(cmd)
	outStr := outBuf.String()
	errStr := errBuf.String()
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return outStr, errStr, exitErr.ExitStatus()
		}
		return outStr, errStr, 1
	}
	return outStr, errStr, 0
}

// upload copies a local file to ~/basename on the remote via SSH stdin.
func upload(client *ssh.Client, localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	remotePath = strings.TrimPrefix(remotePath, "~/")
	base := filepath.Base(remotePath)
	session.Stdin = bytes.NewReader(data)
	return session.Run(fmt.Sprintf("cat > ~/%s && chmod +x ~/%s", base, base))
}

// waitForSSH polls the SSH port until reachable or timeout.
func waitForSSH(host string, port int, d time.Duration) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("SSH did not come back within %v", d)
}

// ── Concurrent helper ─────────────────────────────────────────────────────────

type instResult struct {
	label string
	ok    bool
	msg   string
}

// forAll runs fn concurrently for every instance in the slice and collects results.
// Results are returned sorted by instance label for deterministic output.
func forAll(client *ssh.Client, instances []Instance, fn func(*ssh.Client, Instance) (bool, string)) []instResult {
	ch := make(chan instResult, len(instances))
	for _, inst := range instances {
		inst := inst
		go func() {
			ok, msg := fn(client, inst)
			ch <- instResult{inst.Label, ok, msg}
		}()
	}
	results := make([]instResult, 0, len(instances))
	for range instances {
		results = append(results, <-ch)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].label < results[j].label })
	return results
}

// allOK returns true when every result passed.
func allOK(rs []instResult) bool {
	for _, r := range rs {
		if !r.ok {
			return false
		}
	}
	return true
}

// failMsgs builds a summary string from failed instResults.
func failMsgs(rs []instResult) string {
	var parts []string
	for _, r := range rs {
		if !r.ok {
			parts = append(parts, fmt.Sprintf("[%s] %s", r.label, r.msg))
		}
	}
	return strings.Join(parts, " | ")
}

// ── Test result tracking ──────────────────────────────────────────────────────

type Result struct {
	Num      int
	Name     string
	Passed   bool
	Msg      string
	Duration time.Duration
}

var results []Result

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func pass(num int, name string, d time.Duration) {
	results = append(results, Result{num, name, true, "", d})
	fmt.Printf("[PASS] %02d %s (%s)\n", num, name, fmtDur(d))
}

func fail(num int, name, msg string, d time.Duration) {
	results = append(results, Result{num, name, false, msg, d})
	fmt.Printf("[FAIL] %02d %s — %s (%s)\n", num, name, msg, fmtDur(d))
}

// ── Test suite ────────────────────────────────────────────────────────────────

// cleanupAllInstances tears down any leftover state from previous runs so tests
// always start from a known-clean state.
func cleanupAllInstances(client *ssh.Client) {
	for _, inst := range allInstances {
		run(client, fmt.Sprintf(
			"[ -f %s ] && sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true",
			inst.EnvFile, inst.EnvFile,
		))
		run(client, fmt.Sprintf("sudo rm -rf /run/%sdocker 2>/dev/null; true", inst.Prefix)) // legacy cleanup
		// Clean ZFS child datasets (Docker ZFS driver creates them) but preserve root dataset
		run(client, fmt.Sprintf(
			`if command -v zfs &>/dev/null; then ds=$(df --output=source '%s' 2>/dev/null | tail -1 | tr -d '[:space:]'); if [ -n "$ds" ] && zfs list "$ds" &>/dev/null; then zfs list -r -H -o name "$ds" 2>/dev/null | tail -n +2 | sort -r | while read child; do sudo zfs destroy -f "$child" 2>/dev/null; done; fi; fi; true`,
			inst.Root,
		))
		run(client, fmt.Sprintf("sudo rm -rf %s/* 2>/dev/null; sudo rm -rf %s/.[!.]* 2>/dev/null; true", inst.Root, inst.Root))
		run(client, fmt.Sprintf("sudo ip link delete %sdocker0 2>/dev/null; true", inst.Prefix))
		run(client, fmt.Sprintf(
			"sudo systemctl stop %sdocker 2>/dev/null; sudo systemctl disable %sdocker 2>/dev/null; true",
			inst.Prefix, inst.Prefix,
		))
		run(client, fmt.Sprintf("sudo rm -f /etc/systemd/system/%sdocker.service 2>/dev/null; true", inst.Prefix))
		run(client, fmt.Sprintf("rm -f %s 2>/dev/null; true", inst.EnvFile))
	}
	// Clean up nested-root test instance (test 30)
	run(client, "[ -f ~/dockyard-nested-test/dyn.env ] && sudo env DOCKYARD_ENV=~/dockyard-nested-test/dyn.env ~/dockyard.sh destroy --yes 2>/dev/null; true")
	run(client, "sudo rm -rf /var/tmp/dockyard-nested 2>/dev/null; true")
	run(client, "sudo systemctl stop dyn_docker 2>/dev/null; sudo systemctl disable dyn_docker 2>/dev/null; true")
	run(client, "sudo rm -f /etc/systemd/system/dyn_docker.service 2>/dev/null; true")
	run(client, "rm -rf ~/dockyard-nested-test 2>/dev/null; true")
	// Clean up BTRFS loopback from tests 40-42
	run(client, "sudo umount /var/tmp/btrfs-mount 2>/dev/null; true")
	run(client, "sudo rm -rf /var/tmp/btrfs-mount /var/tmp/btrfs-test.img 2>/dev/null; true")
	run(client, "sudo systemctl daemon-reload 2>/dev/null; true")
}

func runTests(client *ssh.Client, host string, port int, user, keyPath string) {
	// Pre-flight: ensure no leftover state from a previous run
	fmt.Println("[INFO] Pre-flight cleanup (removing any leftover state)...")
	cleanupAllInstances(client)

	//
	// ── Phase 1: Upload & gen-env ─────────────────────────────────────────────
	//

	// 01 — Upload dockyard.sh
	{
		start := time.Now()
		err := upload(client, "dist/dockyard.sh", "~/dockyard.sh")
		d := time.Since(start)
		if err != nil {
			fail(1, "Upload dockyard.sh", err.Error(), d)
			return
		}
		pass(1, "Upload dockyard.sh", d)
	}

	// 02-04 — gen-env for each instance (sequential, cheap)
	for i, inst := range allInstances {
		num := i + 2
		start := time.Now()
		cmd := fmt.Sprintf(
			"rm -f %s && DOCKYARD_ENV=%s DOCKYARD_ROOT=%s DOCKYARD_DOCKER_PREFIX=%s ~/dockyard.sh gen-env",
			inst.EnvFile, inst.EnvFile, inst.Root, inst.Prefix,
		)
		_, se, code := run(client, cmd)
		d := time.Since(start)
		if code != 0 {
			fail(num, fmt.Sprintf("gen-env %s", inst.Label), se, d)
			return
		}
		pass(num, fmt.Sprintf("gen-env %s (%s / %s)", inst.Label, inst.Root, inst.Prefix), d)
	}

	//
	// ── Phase 2: Create all instances concurrently ────────────────────────────
	//

	// 05 — create A + B + C in parallel (3s stagger to avoid dpkg-deb races)
	fmt.Println("[INFO] Creating all instances concurrently (this takes a while)...")
	{
		start := time.Now()
		type createRes struct {
			inst Instance
			ok   bool
			msg  string
		}
		createCh := make(chan createRes, len(allInstances))
		for idx, inst := range allInstances {
			idx, inst := idx, inst
			go func() {
				time.Sleep(time.Duration(idx) * 3 * time.Second)
				_, se, c := run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh create", inst.EnvFile))
				createCh <- createRes{inst, c == 0, se}
			}()
		}
		var createFails []string
		for range allInstances {
			r := <-createCh
			if !r.ok {
				createFails = append(createFails, fmt.Sprintf("[%s] %s", r.inst.Label, r.msg))
			}
		}
		d := time.Since(start)
		if len(createFails) > 0 {
			fail(5, "create all instances", strings.Join(createFails, " | "), d)
			return
		}
		pass(5, "create all instances (A+B+C concurrent)", d)
	}

	//
	// ── Phase 3: Service health ───────────────────────────────────────────────
	//

	// 06 — per-instance docker services active (sysbox is embedded per-instance, not a shared service)
	var rs []instResult
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			_, _, c1 := run(c, "systemctl is-active "+inst.Prefix+"docker")
			if c1 != 0 {
				return false, inst.Prefix + "docker not active"
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(6, "all instances: per-instance docker services active", d)
		} else {
			fail(6, "all instances: services active", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 4: Basic container run ─────────────────────────────────────────
	//

	// 07 — all instances: container runs
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine echo hello", inst.Docker))
			if code != 0 || !strings.Contains(out, "hello") {
				return false, se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(7, "all instances: container run", d)
			// Save alpine:latest to host cache so DinD tests can load it without
			// hitting Docker Hub unauthenticated pull rate limits.
			run(client, fmt.Sprintf(
				"sudo %s save alpine:latest > /var/tmp/alpine.tar",
				allInstances[0].Docker,
			))
		} else {
			fail(7, "all instances: container run", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 5: Networking ───────────────────────────────────────────────────
	//

	// 08 — all instances: outbound ping
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine ping -c3 1.1.1.1", inst.Docker))
			if code != 0 || strings.Contains(out, "100% packet loss") {
				return false, out + se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(8, "all instances: outbound ping", d)
		} else {
			fail(8, "all instances: outbound ping", failMsgs(rs), d)
		}
	}

	// 09 — all instances: DNS resolution
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine nslookup google.com", inst.Docker))
			if code != 0 || !strings.Contains(out, "Address") {
				return false, se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(9, "all instances: DNS resolution", d)
		} else {
			fail(9, "all instances: DNS resolution", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 6: Docker-in-Docker ─────────────────────────────────────────────
	//

	// Pre-pull docker:26.1-dind on all instances concurrently.
	// On a fresh host the image pull can take minutes, which would cause DinD
	// tests to time out. Pulling upfront keeps the test timeouts about container
	// startup, not image download.
	fmt.Println("[INFO] Pre-pulling docker:26.1-dind on all instances...")
	forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
		_, _, _ = run(c, fmt.Sprintf("sudo %s pull docker:26.1-dind", inst.Docker))
		return true, ""
	})

	// 10 — all instances: start DinD container (no --privileged; sysbox handles it)
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			cname := "dind-" + strings.ToLower(inst.Label)
			run(c, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", inst.Docker, cname))
			_, se, code := run(c, fmt.Sprintf("sudo %s run -d --name %s -v /dev/null:/usr/sbin/zfs docker:26.1-dind", inst.Docker, cname))
			if code != 0 {
				return false, se
			}
			// Wait up to 120s for inner dockerd
			for i := 0; i < 60; i++ {
				_, _, c2 := run(c, fmt.Sprintf("sudo %s exec %s docker info", inst.Docker, cname))
				if c2 == 0 {
					// Preload alpine from host cache into inner docker to avoid pull rate limits.
					run(c, fmt.Sprintf(
						"sudo %s exec -i %s docker load < /var/tmp/alpine.tar",
						inst.Docker, cname,
					))
					return true, ""
				}
				time.Sleep(2 * time.Second)
			}
			return false, "inner dockerd did not start within 120s"
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(10, "all instances: DinD start", d)
		} else {
			fail(10, "all instances: DinD start", failMsgs(rs), d)
		}
	}

	// 11 — all instances: DinD inner container
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			cname := "dind-" + strings.ToLower(inst.Label)
			out, se, code := run(c, fmt.Sprintf(
				"sudo %s exec %s docker run --rm alpine echo inner-hello",
				inst.Docker, cname,
			))
			if code != 0 || !strings.Contains(out, "inner-hello") {
				return false, se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(11, "all instances: DinD inner container", d)
		} else {
			fail(11, "all instances: DinD inner container", failMsgs(rs), d)
		}
	}

	// 12 — all instances: DinD inner networking
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			cname := "dind-" + strings.ToLower(inst.Label)
			out, se, code := run(c, fmt.Sprintf(
				"sudo %s exec %s docker run --rm alpine ping -c3 1.1.1.1",
				inst.Docker, cname,
			))
			if code != 0 || strings.Contains(out, "100% packet loss") {
				return false, out + se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(12, "all instances: DinD inner networking", d)
		} else {
			fail(12, "all instances: DinD inner networking", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 7: Multi-instance isolation ────────────────────────────────────
	//

	// 13 — all pairs isolated: A↔B, A↔C, B↔C
	{
		start := time.Now()
		isolationFails := checkIsolation(client, allInstances)
		d := time.Since(start)
		if len(isolationFails) == 0 {
			pass(13, "multi-instance isolation (all pairs)", d)
		} else {
			fail(13, "multi-instance isolation", strings.Join(isolationFails, " | "), d)
		}
	}

	// Cleanup DinD containers before verify and edge-case phases
	for _, inst := range allInstances {
		inst := inst
		cname := "dind-" + strings.ToLower(inst.Label)
		run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", inst.Docker, cname))
	}

	//
	// ── Phase 8: Verify subcommand ────────────────────────────────────────────
	//

	// 14 — all instances: verify subcommand passes end-to-end (concurrent)
	// Runs dockyard.sh verify on each instance; internally exercises service,
	// socket, docker info, container run, outbound networking, and DinD.
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf(
				"DOCKYARD_ENV=%s sudo -E ~/dockyard.sh verify", inst.EnvFile,
			))
			if code != 0 {
				return false, se
			}
			if !strings.Contains(out, "All 6 checks passed.") {
				return false, "unexpected output: " + out
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(14, "all instances: verify passes (6/6 checks)", d)
		} else {
			fail(14, "all instances: verify", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 9: Edge-case tests ──────────────────────────────────────────────
	//

	// 15 — stop/start cycle on instance A
	// Validates ExecStartPre/ExecStopPost iptables lifecycle and clean daemon restart.
	{
		start := time.Now()
		inst := allInstances[0]
		_, se, code := run(client, "sudo systemctl stop "+inst.Prefix+"docker")
		if code != 0 {
			fail(15, "stop/start cycle", "stop failed: "+se, time.Since(start))
		} else {
			_, _, isActive := run(client, "systemctl is-active "+inst.Prefix+"docker")
			if isActive == 0 {
				fail(15, "stop/start cycle", "service still active after stop", time.Since(start))
			} else {
				_, se2, c2 := run(client, "sudo systemctl start "+inst.Prefix+"docker")
				if c2 != 0 {
					fail(15, "stop/start cycle", "start failed: "+se2, time.Since(start))
				} else {
					out, se3, c3 := run(client, fmt.Sprintf(
						"sudo %s run --rm alpine echo cycled", inst.Docker,
					))
					if c3 != 0 || !strings.Contains(out, "cycled") {
						fail(15, "stop/start cycle", "container after restart: "+se3, time.Since(start))
					} else {
						pass(15, "stop/start cycle (stop → start → container run)", time.Since(start))
					}
				}
			}
		}
	}

	// 16 — socket permissions: not world-accessible + owned by instance group
	// The docker socket must never be world-accessible, and must be owned by the
	// per-instance group (${PREFIX}docker) so group members can access it without sudo.
	{
		start := time.Now()
		rs = forAll(client, allInstances, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("stat -c '%%a %%G' %s", inst.Socket))
			if code != 0 {
				return false, "stat failed: " + se
			}
			parts := strings.Fields(strings.TrimSpace(out))
			if len(parts) != 2 {
				return false, "unexpected stat output: " + out
			}
			mode, group := parts[0], parts[1]
			// Last octal digit covers world r/w/x — must be 0.
			if len(mode) > 0 && mode[len(mode)-1] != '0' {
				return false, fmt.Sprintf("socket %s is world-accessible (mode %s)", inst.Socket, mode)
			}
			// Group must be <prefix>docker
			expectedGroup := inst.Prefix + "docker"
			if group != expectedGroup {
				return false, fmt.Sprintf("socket %s group is %q, want %q", inst.Socket, group, expectedGroup)
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(16, "socket permissions (not world-accessible, group-owned by instance)", d)
		} else {
			fail(16, "socket permissions", failMsgs(rs), d)
		}
	}

	// 17 — sysbox bind-mount: mkdir inside non-root-owned 777 dir
	// On kernel 6.17+, sysbox user-namespace containers cannot create entries
	// inside bind-mounted directories owned by UIDs outside the container's
	// mapping, even with 777 permissions. This test catches that regression.
	// The fix is to chown the directory to root (UID 0) before bind-mounting.
	{
		start := time.Now()
		inst := allInstances[0]
		testDir := "/var/tmp/dockyard-bindmount-test"

		// Create a 777 directory owned by the SSH user (non-root UID)
		run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
		run(client, fmt.Sprintf("mkdir -p %s && chmod 777 %s", testDir, testDir))

		// Try mkdir inside a sysbox container with this bind mount
		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm -v %s:/mnt/test alpine sh -c 'mkdir /mnt/test/subdir && echo mkdir-ok'",
			inst.Docker, testDir,
		))
		nonRootOK := code == 0 && strings.Contains(out, "mkdir-ok")

		if !nonRootOK {
			// Verify the workaround: chown to root, then retry
			run(client, fmt.Sprintf("sudo chown 0:0 %s", testDir))
			out, se, code = run(client, fmt.Sprintf(
				"sudo %s run --rm -v %s:/mnt/test alpine sh -c 'mkdir /mnt/test/subdir2 && echo mkdir-ok'",
				inst.Docker, testDir,
			))
			if code != 0 || !strings.Contains(out, "mkdir-ok") {
				run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
				fail(17, "sysbox bind-mount mkdir", "mkdir fails even with root-owned dir: "+se, time.Since(start))
			} else {
				run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
				pass(17, "sysbox bind-mount mkdir (non-root-owned 777 dirs need chown 0:0 workaround)", time.Since(start))
			}
		} else {
			run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
			pass(17, "sysbox bind-mount mkdir (non-root-owned 777 dir works natively)", time.Since(start))
		}
	}

	//
	// ── Phase 10: Destroy instance A, verify B+C unaffected ──────────────────
	//

	// 18 — destroy A under load (running container must not block destroy)
	{
		start := time.Now()
		inst := allInstances[0]
		run(client, fmt.Sprintf(
			"sudo %s run -d --name load-test alpine sleep 300 2>/dev/null",
			inst.Docker,
		))
		_, se, code := run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes", inst.EnvFile))
		d := time.Since(start)
		if code != 0 {
			fail(18, "destroy under load", se, d)
		} else {
			pass(18, "destroy under load (running container present at destroy time)", d)
		}
	}

	// 19 — double destroy idempotency (A already gone, second call must succeed)
	{
		start := time.Now()
		_, se, code := run(client, fmt.Sprintf(
			"sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes", allInstances[0].EnvFile,
		))
		d := time.Since(start)
		if code != 0 {
			fail(19, "double destroy idempotency", se, d)
		} else {
			pass(19, "double destroy idempotency (second destroy is a no-op)", d)
		}
	}

	// 20 — A: service gone, bridge gone, iptables clean
	{
		start := time.Now()
		aClean := true
		var aCleanMsgs []string

		_, _, c1 := run(client, "systemctl is-active "+allInstances[0].Prefix+"docker")
		if c1 == 0 {
			aClean = false
			aCleanMsgs = append(aCleanMsgs, "service still active")
		}
		_, _, c2 := run(client, "ip link show "+allInstances[0].Prefix+"docker0")
		if c2 == 0 {
			aClean = false
			aCleanMsgs = append(aCleanMsgs, "bridge still exists")
		}
		ipt, _, _ := run(client, "iptables-save | grep -F "+allInstances[0].Prefix+" || true")
		if strings.Contains(ipt, allInstances[0].Prefix) {
			aClean = false
			aCleanMsgs = append(aCleanMsgs, "residual iptables rules")
		}
		d := time.Since(start)
		if aClean {
			pass(20, "A: fully cleaned up (service+bridge+iptables)", d)
		} else {
			fail(20, "A: fully cleaned up", strings.Join(aCleanMsgs, ", "), d)
		}
	}

	// 21 — B+C: still healthy after A destroy (container + ping)
	surviving := allInstances[1:]
	{
		start := time.Now()
		rs = forAll(client, surviving, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine ping -c3 1.1.1.1", inst.Docker))
			if code != 0 || strings.Contains(out, "100% packet loss") {
				return false, out + se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(21, "B+C: still healthy after A destroy", d)
		} else {
			fail(21, "B+C: still healthy after A destroy", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 11: Reboot — all surviving instances must come back ─────────────
	//

	// 22 — reboot
	{
		start := time.Now()
		fmt.Println("[INFO] Rebooting host...")
		run(client, "sudo reboot")
		client.Close()
		time.Sleep(15 * time.Second) // wait for it to actually go down

		fmt.Println("[INFO] Waiting for SSH (up to 4min)...")
		if err := waitForSSH(host, port, 4*time.Minute); err != nil {
			fail(22, "reboot", err.Error(), time.Since(start))
			return
		}
		// Give systemd a few seconds to finish starting services
		time.Sleep(10 * time.Second)

		var reconnErr error
		client, reconnErr = dialSSH(host, port, user, keyPath)
		if reconnErr != nil {
			fail(22, "reboot", "could not reconnect: "+reconnErr.Error(), time.Since(start))
			return
		}
		pass(22, "reboot", time.Since(start))
	}

	// 23 — post-reboot: B+C docker services active (concurrent)
	// Retry for up to 90s: cold boot on slow hosts means ExecStartPost (API readiness poll)
	// may still be running when SSH becomes available, causing the service to be in
	// "activating (start-post)" until docker accepts connections.
	{
		start := time.Now()
		rs = forAll(client, surviving, func(c *ssh.Client, inst Instance) (bool, string) {
			deadline := time.Now().Add(90 * time.Second)
			for {
				_, _, c1 := run(c, "systemctl is-active "+inst.Prefix+"docker")
				if c1 == 0 {
					return true, ""
				}
				if time.Now().After(deadline) {
					return false, inst.Prefix + "docker not active"
				}
				time.Sleep(2 * time.Second)
			}
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(23, "post-reboot: B+C services active", d)
		} else {
			fail(23, "post-reboot: B+C services active", failMsgs(rs), d)
		}
	}

	// 24 — post-reboot: B+C containers run (concurrent)
	{
		start := time.Now()
		rs = forAll(client, surviving, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine echo hello", inst.Docker))
			if code != 0 || !strings.Contains(out, "hello") {
				return false, se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(24, "post-reboot: B+C container run", d)
		} else {
			fail(24, "post-reboot: B+C container run", failMsgs(rs), d)
		}
	}

	// 25 — post-reboot: B+C outbound networking (concurrent)
	{
		start := time.Now()
		rs = forAll(client, surviving, func(c *ssh.Client, inst Instance) (bool, string) {
			out, se, code := run(c, fmt.Sprintf("sudo %s run --rm alpine ping -c3 1.1.1.1", inst.Docker))
			if code != 0 || strings.Contains(out, "100% packet loss") {
				return false, out + se
			}
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(25, "post-reboot: B+C outbound networking", d)
		} else {
			fail(25, "post-reboot: B+C outbound networking", failMsgs(rs), d)
		}
	}

	// 26 — post-reboot: B+C DinD full (start + inner container + inner ping, concurrent)
	{
		start := time.Now()
		rs = forAll(client, surviving, func(c *ssh.Client, inst Instance) (bool, string) {
			cname := "dind-post-" + strings.ToLower(inst.Label)
			run(c, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", inst.Docker, cname))

			_, se, code := run(c, fmt.Sprintf("sudo %s run -d --name %s -v /dev/null:/usr/sbin/zfs docker:26.1-dind", inst.Docker, cname))
			if code != 0 {
				return false, "start: " + se
			}
			// Wait for inner dockerd
			ready := false
			for i := 0; i < 60; i++ {
				_, _, c2 := run(c, fmt.Sprintf("sudo %s exec %s docker info", inst.Docker, cname))
				if c2 == 0 {
					ready = true
					// Preload alpine from host cache into inner docker to avoid pull rate limits.
					run(c, fmt.Sprintf(
						"sudo %s exec -i %s docker load < /var/tmp/alpine.tar",
						inst.Docker, cname,
					))
					break
				}
				time.Sleep(2 * time.Second)
			}
			if !ready {
				return false, "inner dockerd did not start within 120s"
			}
			// Inner container
			out, se, code := run(c, fmt.Sprintf(
				"sudo %s exec %s docker run --rm alpine echo inner-hello",
				inst.Docker, cname,
			))
			if code != 0 || !strings.Contains(out, "inner-hello") {
				return false, "inner container: " + se
			}
			// Inner networking
			out, se, code = run(c, fmt.Sprintf(
				"sudo %s exec %s docker run --rm alpine ping -c3 1.1.1.1",
				inst.Docker, cname,
			))
			if code != 0 || strings.Contains(out, "100% packet loss") {
				return false, "inner networking: " + out + se
			}
			run(c, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", inst.Docker, cname))
			return true, ""
		})
		d := time.Since(start)
		if allOK(rs) {
			pass(26, "post-reboot: B+C DinD (start + inner container + inner networking)", d)
		} else {
			fail(26, "post-reboot: B+C DinD", failMsgs(rs), d)
		}
	}

	//
	// ── Phase 12: Destroy remaining instances ─────────────────────────────────
	//

	// 27-28 — destroy B and C sequentially (avoid systemd race)
	for i, inst := range surviving {
		num := 27 + i
		start := time.Now()
		_, se, code := run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes", inst.EnvFile))
		d := time.Since(start)
		if code != 0 {
			fail(num, fmt.Sprintf("destroy %s", inst.Label), se, d)
		} else {
			pass(num, fmt.Sprintf("destroy %s", inst.Label), d)
		}
	}

	// 29 — full cleanup: no services, bridges, iptables rules, data dirs, users
	{
		start := time.Now()
		var cleanFails []string
		for _, inst := range surviving {
			// per-instance docker service
			_, _, c := run(client, "systemctl is-active "+inst.Prefix+"docker")
			if c == 0 {
				cleanFails = append(cleanFails, inst.Label+": docker service still active")
			}
			// bridge
			_, _, c = run(client, "ip link show "+inst.Prefix+"docker0")
			if c == 0 {
				cleanFails = append(cleanFails, inst.Label+": bridge still exists")
			}
			// iptables
			ipt, _, _ := run(client, "iptables-save | grep -F "+inst.Prefix+" || true")
			if strings.Contains(ipt, inst.Prefix) {
				cleanFails = append(cleanFails, inst.Label+": residual iptables rules")
			}
			// data directory (instance root) — on ZFS the mountpoint directory persists
			// but must be empty; on other filesystems it must be gone entirely
			out, _, _ := run(client, fmt.Sprintf("[ -d %s ] && echo exists || echo gone", inst.Root))
			if strings.TrimSpace(out) == "exists" {
				// Check if it's an empty ZFS mountpoint (acceptable) vs leftover data (failure)
				countOut, _, _ := run(client, fmt.Sprintf("ls -A %s 2>/dev/null | wc -l", inst.Root))
				if strings.TrimSpace(countOut) != "0" {
					cleanFails = append(cleanFails, inst.Label+": "+inst.Root+" still has contents after destroy")
				}
			}
			// per-instance sysbox run dir gone (run/sysbox inside instance root)
			out, _, _ = run(client, fmt.Sprintf("[ -d %s/run/sysbox ] && echo exists || echo gone", inst.Root))
			if strings.TrimSpace(out) == "exists" {
				cleanFails = append(cleanFails, inst.Label+": "+inst.Root+"/run/sysbox still exists")
			}
		}
		// instance users and groups removed
		for _, inst := range surviving {
			instanceUser := inst.Prefix + "docker"
			_, _, cu := run(client, "getent passwd "+instanceUser)
			if cu == 0 {
				cleanFails = append(cleanFails, inst.Label+": system user "+instanceUser+" still exists")
			}
			_, _, cg := run(client, "getent group "+instanceUser)
			if cg == 0 {
				cleanFails = append(cleanFails, inst.Label+": system group "+instanceUser+" still exists")
			}
		}
		d := time.Since(start)
		if len(cleanFails) == 0 {
			pass(29, "full cleanup: no services, bridges, iptables, data dirs, or users", d)
		} else {
			fail(29, "full cleanup", strings.Join(cleanFails, " | "), d)
		}
	}

	//
	// ── Phase 13: Nested DOCKYARD_ROOT lifecycle ──────────────────────────────
	//

	// 30 — deeply nested DOCKYARD_ROOT: gen-env → create → container run → destroy
	// Verifies the FHS layout works when DOCKYARD_ROOT is several levels deep.
	{
		start := time.Now()
		nestedRoot := "/var/tmp/dockyard-nested/level1/level2/dockyard"
		nestedPrefix := "dyn_"
		nestedEnv := "~/dockyard-nested-test/dyn.env"
		nestedSocket := nestedRoot + "/run/docker.sock"
		nestedDocker := nestedRoot + "/bin/docker"
		_ = nestedSocket // used only for reference; docker commands use nestedDocker

		// Pre-cleanup in case a previous run left state
		run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true", nestedEnv))
		run(client, fmt.Sprintf("sudo rm -rf /var/tmp/dockyard-nested 2>/dev/null; true"))
		run(client, "rm -rf ~/dockyard-nested-test 2>/dev/null; true")
		run(client, fmt.Sprintf("sudo systemctl stop %sdocker 2>/dev/null; sudo systemctl disable %sdocker 2>/dev/null; true", nestedPrefix, nestedPrefix))
		run(client, fmt.Sprintf("sudo rm -f /etc/systemd/system/%sdocker.service 2>/dev/null; true", nestedPrefix))
		run(client, "sudo systemctl daemon-reload 2>/dev/null; true")

		nestedOK := true
		var nestedMsg string

		run(client, "mkdir -p ~/dockyard-nested-test")
		_, se, code := run(client, fmt.Sprintf(
			"DOCKYARD_ENV=%s DOCKYARD_ROOT=%s DOCKYARD_DOCKER_PREFIX=%s ~/dockyard.sh gen-env",
			nestedEnv, nestedRoot, nestedPrefix,
		))
		if code != 0 {
			nestedOK, nestedMsg = false, "gen-env: "+se
		}

		if nestedOK {
			_, se, code = run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh create", nestedEnv))
			if code != 0 {
				nestedOK, nestedMsg = false, "create: "+se
			}
		}

		if nestedOK {
			// Preload alpine from host cache to avoid Docker Hub rate limits.
			run(client, fmt.Sprintf("sudo %s load < /var/tmp/alpine.tar", nestedDocker))
			out, se, code := run(client, fmt.Sprintf(
				"sudo %s run --rm alpine echo nested-ok",
				nestedDocker,
			))
			if code != 0 || !strings.Contains(out, "nested-ok") {
				nestedOK, nestedMsg = false, "container run: "+se
			}
		}

		if nestedOK {
			_, se, code = run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes", nestedEnv))
			if code != 0 {
				nestedOK, nestedMsg = false, "destroy: "+se
			} else {
				out, _, _ := run(client, fmt.Sprintf("[ -d %s ] && echo exists || echo gone", nestedRoot))
				if strings.TrimSpace(out) == "exists" {
					nestedOK, nestedMsg = false, nestedRoot+" still exists after destroy"
				}
			}
		}

		// Always clean up, even on failure
		run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true", nestedEnv))
		run(client, "sudo rm -rf /var/tmp/dockyard-nested 2>/dev/null; true")
		run(client, "rm -rf ~/dockyard-nested-test 2>/dev/null; true")

		d := time.Since(start)
		if nestedOK {
			pass(30, "nested DOCKYARD_ROOT lifecycle (gen-env + create + container run + destroy)", d)
		} else {
			fail(30, "nested DOCKYARD_ROOT lifecycle", nestedMsg, d)
		}
	}

	//
	// ── Phase 14: isolation.d iptables mechanism ─────────────────────────────
	//
	// Tests T1-T10 from multi_tenant.md. Uses a fresh instance A (was
	// destroyed in phase 10). We re-create it here so the isolation tests
	// are self-contained.

	instA := allInstances[0]
	isoChainA := "DOCKYARD-ISO-" + strings.TrimSuffix(instA.Prefix, "_")
	isoOK := true // gates subsequent isolation tests

	// BTRFS test variables (declared here to avoid goto-over-declaration)
	btrfsOK := true
	btrfsImg := "/var/tmp/btrfs-test.img"
	btrfsMnt := "/var/tmp/btrfs-mount"

	fmt.Println("[INFO] Re-creating instance A for isolation.d tests...")
	{
		_, se, c := run(client, fmt.Sprintf(
			"rm -f %s && DOCKYARD_ENV=%s DOCKYARD_ROOT=%s DOCKYARD_DOCKER_PREFIX=%s ~/dockyard.sh gen-env",
			instA.EnvFile, instA.EnvFile, instA.Root, instA.Prefix,
		))
		if c != 0 {
			fail(31, "isolation.d: re-create instance A (gen-env)", se, 0)
			isoOK = false
			goto done
		}
		_, se, c = run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh create", instA.EnvFile))
		if c != 0 {
			fail(31, "isolation.d: re-create instance A (create)", se, 0)
			isoOK = false
			goto done
		}
		// Preload alpine from host cache
		run(client, fmt.Sprintf("sudo %s load < /var/tmp/alpine.tar 2>/dev/null; true", instA.Docker))
	}

	// 31 — T1+T2: chain created when rules exist, absent otherwise
	if isoOK {
		start := time.Now()
		instB := allInstances[1]
		isoChainB := "DOCKYARD-ISO-" + strings.TrimSuffix(instB.Prefix, "_")

		var msgs []string

		// Write a dummy rules file to A's isolation.d
		_, se, c := run(client, fmt.Sprintf(
			"sudo mkdir -p %s/etc/isolation.d && echo '10.99.99.1' | sudo tee %s/etc/isolation.d/test.rules >/dev/null",
			instA.Root, instA.Root,
		))
		if c != 0 {
			msgs = append(msgs, "write rules: "+se)
		}

		// Create a user-defined network so ExecStartPost has a br-* bridge to attach to
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo %s network create iso-trigger-net 2>/dev/null; true", instA.Docker))
		}

		// Restart A's service to pick up the rules
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
			_, se, c = run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
			if c != 0 {
				msgs = append(msgs, "restart A: "+se)
			}
			time.Sleep(5 * time.Second)
		}

		// T1a: Chain exists
		if len(msgs) == 0 {
			out, _, c := run(client, fmt.Sprintf("sudo iptables -L %s -n 2>&1", isoChainA))
			if c != 0 {
				msgs = append(msgs, "chain not created: "+out)
			}
		}

		// T1b: Chain contains ACCEPT for the IP
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L %s -n", isoChainA))
			if !strings.Contains(out, "10.99.99.1") {
				msgs = append(msgs, "chain missing ACCEPT for 10.99.99.1")
			}
		}

		// T1c: Chain ends with DROP
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L %s -n", isoChainA))
			lines := strings.Split(strings.TrimSpace(out), "\n")
			lastLine := lines[len(lines)-1]
			if !strings.Contains(lastLine, "DROP") {
				msgs = append(msgs, "chain does not end with DROP (last line: "+lastLine+")")
			}
		}

		// T1d: FORWARD jump rule exists scoped to a br-* bridge
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L FORWARD -n -v | grep %s || true", isoChainA))
			if !strings.Contains(out, "br-") {
				msgs = append(msgs, "no bridge-scoped FORWARD jump rule")
			}
		}

		// T2: Instance B has no rules files → no isolation chain
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainB))
			if c == 0 {
				msgs = append(msgs, "B has isolation chain but no rules files")
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(31, "isolation.d: chain created with rules, absent without (T1+T2)", d)
		} else {
			fail(31, "isolation.d: chain creation", strings.Join(msgs, " | "), d)
			isoOK = false
		}
	}

	// 32 — T3+T4: allowed IPs pass, non-allowed IPs blocked
	//
	// Strategy: create a user-defined network with a known subnet (--subnet),
	// assign static IPs to containers, write the infra IP to rules BEFORE
	// creating the network, so only one restart is needed.
	if isoOK {
		start := time.Now()
		docker := instA.Docker
		var msgs []string

		// Use a fixed subnet so container IPs are predictable
		isoSubnet := "10.88.77.0/24"
		infraIP := "10.88.77.10"
		tenantAIP := "10.88.77.20"
		tenantBIP := "10.88.77.30"

		// Write infra IP to rules and restart FIRST (before creating containers)
		run(client, fmt.Sprintf(
			"echo '%s' | sudo tee %s/etc/isolation.d/test.rules >/dev/null",
			infraIP, instA.Root,
		))
		run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
		_, se, c := run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
		if c != 0 {
			msgs = append(msgs, "restart: "+se)
		} else {
			time.Sleep(5 * time.Second)
		}

		// Create network with fixed subnet
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo %s network rm iso-test-net 2>/dev/null; true", docker))
			_, se, c = run(client, fmt.Sprintf(
				"sudo %s network create --subnet %s iso-test-net",
				docker, isoSubnet,
			))
			if c != 0 {
				msgs = append(msgs, "create network: "+se)
			}
		}

		// Start 3 containers with static IPs
		if len(msgs) == 0 {
			for _, ct := range []struct{ name, ip string }{
				{"infra", infraIP}, {"tenant-a", tenantAIP}, {"tenant-b", tenantBIP},
			} {
				run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", docker, ct.name))
				_, se, c = run(client, fmt.Sprintf(
					"sudo %s run -d --name %s --network iso-test-net --ip %s alpine sleep 300",
					docker, ct.name, ct.ip,
				))
				if c != 0 {
					msgs = append(msgs, "start "+ct.name+": "+se)
				}
			}
		}

		// The new network created a br-* bridge. We need to add the jump rule
		// for it. Restart the service to pick up the new bridge.
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
			_, se, c = run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
			if c != 0 {
				msgs = append(msgs, "restart for bridge jump: "+se)
			} else {
				time.Sleep(5 * time.Second)
			}
			// Recreate containers (restart killed them), same static IPs
			for _, ct := range []struct{ name, ip string }{
				{"infra", infraIP}, {"tenant-a", tenantAIP}, {"tenant-b", tenantBIP},
			} {
				run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", docker, ct.name))
				run(client, fmt.Sprintf(
					"sudo %s run -d --name %s --network iso-test-net --ip %s alpine sleep 300",
					docker, ct.name, ct.ip,
				))
			}
		}

		// T3: tenant-a can reach infra (infra IP is in the allow-list)
		if len(msgs) == 0 {
			out, _, c := run(client, fmt.Sprintf(
				"sudo %s exec tenant-a ping -c2 -W3 %s", docker, infraIP,
			))
			if c != 0 || strings.Contains(out, "100% packet loss") {
				msgs = append(msgs, "tenant-a cannot reach infra ("+infraIP+")")
			}
		}

		// T4: tenant-a CAN reach tenant-b (both on same bridge subnet → auto-whitelisted)
		if len(msgs) == 0 {
			out, _, c := run(client, fmt.Sprintf(
				"sudo %s exec tenant-a ping -c2 -W3 %s", docker, tenantBIP,
			))
			if c != 0 || strings.Contains(out, "100% packet loss") {
				msgs = append(msgs, "tenant-a cannot reach tenant-b ("+tenantBIP+") — same-bridge traffic should pass")
			}
		}

		// Leave containers and network for subsequent tests
		d := time.Since(start)
		if len(msgs) == 0 {
			pass(32, "isolation.d: allowed IPs pass, same-bridge peers communicate (T3+T4)", d)
		} else {
			fail(32, "isolation.d: traffic filtering", strings.Join(msgs, " | "), d)
			isoOK = false
		}
	}

	// 33 — T11: sidecar pattern — two non-whitelisted containers on a
	//          dedicated bridge can communicate (same-bridge auto-whitelist)
	//
	// Reproduces the Tailscale sidecar bug: a sidecar and sandbox container
	// are placed on a per-user bridge. Neither IP is in .rules, but they
	// must be able to reach each other because they share the same L2 segment.
	if isoOK {
		start := time.Now()
		docker := instA.Docker
		var msgs []string

		sidecarNet := "sidecar-test-net"
		sidecarSubnet := "10.88.88.0/24"
		sidecarIP := "10.88.88.2"
		sandboxIP := "10.88.88.3"

		// Create a dedicated bridge (like sc-ts-net-{user})
		run(client, fmt.Sprintf("sudo %s network rm %s 2>/dev/null; true", docker, sidecarNet))
		_, se, c := run(client, fmt.Sprintf(
			"sudo %s network create --subnet %s %s",
			docker, sidecarSubnet, sidecarNet,
		))
		if c != 0 {
			msgs = append(msgs, "create network: "+se)
		}

		// Restart to pick up new bridge and apply isolation rules
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
			_, se, c = run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
			if c != 0 {
				msgs = append(msgs, "restart: "+se)
			} else {
				time.Sleep(5 * time.Second)
			}
		}

		// Start two containers — neither IP is in any .rules file
		if len(msgs) == 0 {
			for _, ct := range []struct{ name, ip string }{
				{"sidecar", sidecarIP}, {"sandbox", sandboxIP},
			} {
				run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", docker, ct.name))
				_, se, c = run(client, fmt.Sprintf(
					"sudo %s run -d --name %s --network %s --ip %s alpine sleep 300",
					docker, ct.name, sidecarNet, ct.ip,
				))
				if c != 0 {
					msgs = append(msgs, "start "+ct.name+": "+se)
				}
			}
		}

		// Sidecar must be able to reach sandbox (same bridge)
		if len(msgs) == 0 {
			out, _, c := run(client, fmt.Sprintf(
				"sudo %s exec sidecar ping -c2 -W3 %s", docker, sandboxIP,
			))
			if c != 0 || strings.Contains(out, "100% packet loss") {
				msgs = append(msgs, "sidecar cannot reach sandbox — same-bridge traffic blocked")
			}
		}

		// Sandbox must be able to reach sidecar (reverse direction)
		if len(msgs) == 0 {
			out, _, c := run(client, fmt.Sprintf(
				"sudo %s exec sandbox ping -c2 -W3 %s", docker, sidecarIP,
			))
			if c != 0 || strings.Contains(out, "100% packet loss") {
				msgs = append(msgs, "sandbox cannot reach sidecar — same-bridge traffic blocked")
			}
		}

		// Clean up
		run(client, fmt.Sprintf("sudo %s rm -f sidecar sandbox 2>/dev/null", docker))
		run(client, fmt.Sprintf("sudo %s network rm %s 2>/dev/null; true", docker, sidecarNet))

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(33, "isolation.d: sidecar pattern — same-bridge non-whitelisted peers communicate (T11)", d)
		} else {
			fail(33, "isolation.d: sidecar pattern", strings.Join(msgs, " | "), d)
			isoOK = false
		}
	}

	// 34 — T5: isolation chain cleaned up on stop, recreated on start
	if isoOK {
		start := time.Now()
		var msgs []string

		// Stop instance A
		_, se, c := run(client, fmt.Sprintf("sudo systemctl stop %sdocker", instA.Prefix))
		if c != 0 {
			msgs = append(msgs, "stop: "+se)
		}

		// Chain should be gone
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainA))
			if c == 0 {
				msgs = append(msgs, "chain still exists after stop")
			}
		}

		// No FORWARD rules should reference the chain
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L FORWARD -n | grep %s || true", isoChainA))
			if strings.Contains(out, isoChainA) {
				msgs = append(msgs, "FORWARD still references chain after stop")
			}
		}

		// Start instance A again
		if len(msgs) == 0 {
			run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
			_, se, c = run(client, fmt.Sprintf("sudo systemctl start %sdocker", instA.Prefix))
			if c != 0 {
				msgs = append(msgs, "start: "+se)
			} else {
				time.Sleep(5 * time.Second)
			}
		}

		// Chain should be recreated (rules file still present)
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainA))
			if c != 0 {
				msgs = append(msgs, "chain not recreated after start")
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(34, "isolation.d: chain cleaned up on stop, recreated on start (T5)", d)
		} else {
			fail(34, "isolation.d: stop/start chain lifecycle", strings.Join(msgs, " | "), d)
			isoOK = false
		}
	}

	// 35 — T6: multiple rules files merged
	if isoOK {
		start := time.Now()
		var msgs []string

		ip1 := "10.88.77.10"
		ip2 := "10.88.77.20"

		// Write two separate rules files
		run(client, fmt.Sprintf(
			"echo '%s' | sudo tee %s/etc/isolation.d/infra.rules >/dev/null",
			ip1, instA.Root,
		))
		run(client, fmt.Sprintf(
			"echo '%s' | sudo tee %s/etc/isolation.d/extra.rules >/dev/null",
			ip2, instA.Root,
		))
		// Remove old test.rules to keep things clean
		run(client, fmt.Sprintf("sudo rm -f %s/etc/isolation.d/test.rules", instA.Root))

		// Restart to apply
		run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
		_, se, c := run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
		if c != 0 {
			msgs = append(msgs, "restart: "+se)
		} else {
			time.Sleep(5 * time.Second)
		}

		// Verify chain contains ACCEPT for both IPs
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L %s -n", isoChainA))
			if !strings.Contains(out, ip1) {
				msgs = append(msgs, "chain missing ACCEPT for "+ip1)
			}
			if !strings.Contains(out, ip2) {
				msgs = append(msgs, "chain missing ACCEPT for "+ip2)
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(35, "isolation.d: multiple rules files merged (T6)", d)
		} else {
			fail(35, "isolation.d: multiple rules files", strings.Join(msgs, " | "), d)
		}
	}

	// 36 — T7: comments and blank lines in rules files ignored
	if isoOK {
		start := time.Now()
		var msgs []string

		// Write a rules file with comments, blank lines, indented comments
		rulesContent := `# This is a comment
10.0.0.1

  # Indented comment
10.0.0.2
`
		run(client, fmt.Sprintf(
			"echo '%s' | sudo tee %s/etc/isolation.d/test-comments.rules >/dev/null",
			rulesContent, instA.Root,
		))
		// Remove other rules files so only this one is active
		run(client, fmt.Sprintf("sudo rm -f %s/etc/isolation.d/infra.rules %s/etc/isolation.d/extra.rules",
			instA.Root, instA.Root))

		// Restart to apply
		run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
		_, se, c := run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
		if c != 0 {
			msgs = append(msgs, "restart: "+se)
		} else {
			time.Sleep(5 * time.Second)
		}

		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L %s -n", isoChainA))
			// Should have ACCEPT for 10.0.0.1 and 10.0.0.2
			if !strings.Contains(out, "10.0.0.1") {
				msgs = append(msgs, "missing ACCEPT for 10.0.0.1")
			}
			if !strings.Contains(out, "10.0.0.2") {
				msgs = append(msgs, "missing ACCEPT for 10.0.0.2")
			}
			// Should NOT contain # artifacts
			if strings.Contains(out, "#") {
				msgs = append(msgs, "comment artifacts in chain rules")
			}
			// Count ACCEPT rules: should be exactly 4 (src+dst for each IP)
			// plus ESTABLISHED,RELATED. Lines with ACCEPT:
			acceptCount := 0
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "ACCEPT") {
					acceptCount++
				}
			}
			// Expected: 1 (ESTABLISHED,RELATED) + 2 (src 10.0.0.1) + 2 (dst 10.0.0.1) wait no...
			// Actually: ESTABLISHED,RELATED + src 10.0.0.1 + dst 10.0.0.1 + src 10.0.0.2 + dst 10.0.0.2 = 5
			if acceptCount != 5 {
				msgs = append(msgs, fmt.Sprintf("expected 5 ACCEPT rules, got %d", acceptCount))
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(36, "isolation.d: comments and blank lines ignored (T7)", d)
		} else {
			fail(36, "isolation.d: comment parsing", strings.Join(msgs, " | "), d)
		}
	}

	// 37 — T8: cross-instance isolation preserved with isolation.d
	//
	// With isolation.d enabled on A, verify A's containers are not visible
	// via the system docker (if installed) or any other Docker socket.
	if isoOK {
		start := time.Now()
		var msgs []string

		// Start a uniquely-named container in A
		run(client, fmt.Sprintf("sudo %s rm -f iso-cross-a 2>/dev/null", instA.Docker))
		_, se, c := run(client, fmt.Sprintf(
			"sudo %s run -d --name iso-cross-a alpine sleep 60", instA.Docker,
		))
		if c != 0 {
			msgs = append(msgs, "start container on A: "+se)
		}

		// Verify A's container is NOT visible via system docker (if installed)
		if len(msgs) == 0 {
			_, _, sysCode := run(client, "which docker")
			if sysCode == 0 {
				out, _, _ := run(client, "sudo docker ps -a --format '{{.Names}}' 2>/dev/null")
				if strings.Contains(out, "iso-cross-a") {
					msgs = append(msgs, "container iso-cross-a visible in system docker ps")
				}
			}
		}

		// Verify A's socket is not the default docker socket
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("ls -la %s/run/docker.sock", instA.Root))
			if strings.Contains(out, "/var/run/docker.sock") {
				msgs = append(msgs, "A's socket is the default docker socket")
			}
		}

		// Verify A can see its own container
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf(
				"sudo %s ps -a --format '{{.Names}}'", instA.Docker,
			))
			if !strings.Contains(out, "iso-cross-a") {
				msgs = append(msgs, "container iso-cross-a not visible in A's own docker ps")
			}
		}

		run(client, fmt.Sprintf("sudo %s rm -f iso-cross-a 2>/dev/null", instA.Docker))

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(37, "isolation.d: daemon isolation preserved (T8)", d)
		} else {
			fail(37, "isolation.d: cross-instance isolation", strings.Join(msgs, " | "), d)
		}
	}

	// 38 — T9: destroy cleans up isolation chain and rules directory
	if isoOK {
		start := time.Now()
		var msgs []string

		// Destroy instance A
		_, se, c := run(client, fmt.Sprintf(
			"sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes", instA.EnvFile,
		))
		if c != 0 {
			msgs = append(msgs, "destroy: "+se)
		}

		// Chain should be gone
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainA))
			if c == 0 {
				msgs = append(msgs, "chain still exists after destroy")
			}
		}

		// isolation.d directory should be gone (part of DOCKYARD_ROOT)
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf(
				"[ -d %s/etc/isolation.d ] && echo exists || echo gone", instA.Root,
			))
			if strings.TrimSpace(out) == "exists" {
				msgs = append(msgs, "isolation.d directory still exists after destroy")
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(38, "isolation.d: destroy cleans up chain and rules dir (T9)", d)
		} else {
			fail(38, "isolation.d: destroy cleanup", strings.Join(msgs, " | "), d)
		}
	}

	// 39 — T10: isolation survives reboot
	//
	// Re-create instance A with isolation rules, reboot the host, verify
	// the chain is automatically recreated by the systemd service.
	if isoOK {
		start := time.Now()
		var msgs []string

		// Re-create instance A
		run(client, fmt.Sprintf(
			"rm -f %s && DOCKYARD_ENV=%s DOCKYARD_ROOT=%s DOCKYARD_DOCKER_PREFIX=%s ~/dockyard.sh gen-env",
			instA.EnvFile, instA.EnvFile, instA.Root, instA.Prefix,
		))
		_, se, c := run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh create", instA.EnvFile))
		if c != 0 {
			msgs = append(msgs, "create: "+se)
		}

		// Write rules and create a user-defined network for a br-* bridge
		if len(msgs) == 0 {
			run(client, fmt.Sprintf(
				"sudo mkdir -p %s/etc/isolation.d && echo '10.99.99.1' | sudo tee %s/etc/isolation.d/reboot-test.rules >/dev/null",
				instA.Root, instA.Root,
			))
			run(client, fmt.Sprintf("sudo %s network create iso-reboot-net 2>/dev/null; true", instA.Docker))
			run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", instA.Prefix))
			_, se, c = run(client, fmt.Sprintf("sudo systemctl restart %sdocker", instA.Prefix))
			if c != 0 {
				msgs = append(msgs, "restart: "+se)
			} else {
				time.Sleep(5 * time.Second)
			}
		}

		// Verify chain exists before reboot
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainA))
			if c != 0 {
				msgs = append(msgs, "chain not created before reboot")
			}
		}

		// Reboot
		if len(msgs) == 0 {
			fmt.Println("[INFO] Rebooting host for isolation reboot test...")
			run(client, "sudo reboot")
			client.Close()
			time.Sleep(15 * time.Second)

			fmt.Println("[INFO] Waiting for SSH (up to 4min)...")
			if err := waitForSSH(host, port, 4*time.Minute); err != nil {
				msgs = append(msgs, "reboot: "+err.Error())
			} else {
				time.Sleep(10 * time.Second)
				var reconnErr error
				client, reconnErr = dialSSH(host, port, user, keyPath)
				if reconnErr != nil {
					msgs = append(msgs, "reconnect: "+reconnErr.Error())
				}
			}
		}

		// Wait for A's service to be active (up to 90s)
		if len(msgs) == 0 {
			deadline := time.Now().Add(90 * time.Second)
			active := false
			for time.Now().Before(deadline) {
				_, _, c = run(client, "systemctl is-active "+instA.Prefix+"docker")
				if c == 0 {
					active = true
					break
				}
				time.Sleep(2 * time.Second)
			}
			if !active {
				msgs = append(msgs, "service not active after reboot")
			}
		}

		// Verify chain is recreated
		if len(msgs) == 0 {
			_, _, c = run(client, fmt.Sprintf("sudo iptables -L %s -n 2>/dev/null", isoChainA))
			if c != 0 {
				msgs = append(msgs, "chain not recreated after reboot")
			}
		}

		// Verify rules content matches
		if len(msgs) == 0 {
			out, _, _ := run(client, fmt.Sprintf("sudo iptables -L %s -n", isoChainA))
			if !strings.Contains(out, "10.99.99.1") {
				msgs = append(msgs, "chain missing ACCEPT for 10.99.99.1 after reboot")
			}
			if !strings.Contains(out, "DROP") {
				msgs = append(msgs, "chain missing DROP rule after reboot")
			}
		}

		d := time.Since(start)
		if len(msgs) == 0 {
			pass(39, "isolation.d: chain survives reboot (T10)", d)
		} else {
			fail(39, "isolation.d: reboot persistence", strings.Join(msgs, " | "), d)
		}
	}

	//
	// ── Phase 16: BTRFS bind mount with sysbox ──────────────────────────────
	//
	// Verifies that sysbox containers can use bind mounts from BTRFS
	// filesystems without EOVERFLOW. Uses a loopback BTRFS image.
	// See: https://github.com/thieso2/dockyard/issues/18
	//      https://github.com/thieso2/sysbox/issues/12

	fmt.Println("[INFO] Setting up BTRFS loopback for bind mount tests...")
	{
		// Check if mkfs.btrfs is available; skip BTRFS tests if not
		_, _, mkfsCode := run(client, "which mkfs.btrfs")
		if mkfsCode != 0 {
			fmt.Println("[SKIP] mkfs.btrfs not found — skipping BTRFS tests (39-41)")
			btrfsOK = false
		}
	}

	if btrfsOK {
		// Create loopback BTRFS filesystem
		_, se, code := run(client, fmt.Sprintf(
			"sudo truncate -s 1G %s && sudo mkfs.btrfs -f %s && "+
				"sudo mkdir -p %s && sudo mount %s %s",
			btrfsImg, btrfsImg, btrfsMnt, btrfsImg, btrfsMnt,
		))
		if code != 0 {
			fmt.Printf("[SKIP] BTRFS setup failed: %s — skipping tests 40-42\n", strings.TrimSpace(se))
			btrfsOK = false
		}
	}

	// 39 — BTRFS bind mount with standard runc (baseline — should always work)
	if btrfsOK {
		start := time.Now()
		testDir := btrfsMnt + "/runc-test"
		run(client, fmt.Sprintf("sudo mkdir -p %s && sudo chmod 777 %s", testDir, testDir))

		// runc doesn't use ID-mapped mounts, so BTRFS works fine
		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm --runtime=runc -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-runc-ok'",
			instA.Docker, testDir,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-runc-ok") {
			pass(40, "BTRFS bind mount with runc (baseline)", d)
		} else {
			fail(40, "BTRFS bind mount with runc (baseline)", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
			btrfsOK = false
		}
		run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
	}

	// 40 — BTRFS bind mount with sysbox-runc (the bug case: EOVERFLOW on chmod)
	// Sysbox uses ID-mapped mounts; BTRFS_SUPER_MAGIC was missing from the
	// blacklist, causing chmod on the mountpoint to fail with EOVERFLOW.
	// See: https://github.com/thieso2/sysbox/issues/12
	if btrfsOK {
		start := time.Now()
		testDir := btrfsMnt + "/sysbox-test"
		run(client, fmt.Sprintf("sudo mkdir -p %s && sudo chmod 777 %s", testDir, testDir))

		// The exact repro: chmod on the bind-mounted dir triggers EOVERFLOW
		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-sysbox-ok'",
			instA.Docker, testDir,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-sysbox-ok") {
			pass(41, "BTRFS bind mount with sysbox (chmod+touch on mountpoint)", d)
		} else {
			fail(41, "BTRFS bind mount with sysbox (chmod+touch on mountpoint)", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
		}
		run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
	}

	// 41 — BTRFS subvolume bind mount with sysbox-runc
	if btrfsOK {
		start := time.Now()
		subvol := btrfsMnt + "/subvol-test"

		// Create a BTRFS subvolume as the mount source
		run(client, fmt.Sprintf("sudo btrfs subvolume create %s 2>/dev/null || sudo mkdir -p %s", subvol, subvol))
		run(client, fmt.Sprintf("sudo chmod 777 %s", subvol))

		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-subvol-ok'",
			instA.Docker, subvol,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-subvol-ok") {
			pass(42, "BTRFS subvolume bind mount with sysbox", d)
		} else {
			fail(42, "BTRFS subvolume bind mount with sysbox", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
		}

		// Clean up: delete subvolume (or dir fallback), then unmount + remove image
		run(client, fmt.Sprintf("sudo btrfs subvolume delete %s 2>/dev/null; sudo rm -rf %s 2>/dev/null; true", subvol, subvol))
	}

	// BTRFS cleanup
	if btrfsOK {
		run(client, fmt.Sprintf("sudo umount %s 2>/dev/null; true", btrfsMnt))
		run(client, fmt.Sprintf("sudo rm -rf %s %s 2>/dev/null; true", btrfsMnt, btrfsImg))
	}

	// Final cleanup: destroy re-created instance A
	run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true", instA.EnvFile))

done:
	_ = isoOK
}

// ── Focused test: BTRFS ──────────────────────────────────────────────────────

// runBtrfsOnly creates a single dockyard instance and runs only the BTRFS
// bind mount tests (39-41). Use with --only btrfs for quick validation on
// a specific kernel/distro without running the full 41-test suite.
func runBtrfsOnly(client *ssh.Client) {
	inst := allInstances[0]

	// Cleanup any leftovers
	fmt.Println("[INFO] Cleaning up any leftover state...")
	run(client, fmt.Sprintf(
		"[ -f %s ] && sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true",
		inst.EnvFile, inst.EnvFile,
	))
	// Kill any orphaned sysbox/docker processes and clear runtime state
	run(client, "sudo pkill -9 sysbox-fs 2>/dev/null; sudo pkill -9 sysbox-mgr 2>/dev/null; sudo pkill -9 containerd 2>/dev/null; true")
	run(client, "sleep 1; true")
	run(client, fmt.Sprintf("sudo rm -rf %s/* %s/.[!.]* 2>/dev/null; true", inst.Root, inst.Root))
	run(client, fmt.Sprintf("sudo ip link delete %sdocker0 2>/dev/null; true", inst.Prefix))
	run(client, fmt.Sprintf("sudo systemctl stop %sdocker 2>/dev/null; sudo systemctl disable %sdocker 2>/dev/null; true", inst.Prefix, inst.Prefix))
	run(client, fmt.Sprintf("sudo rm -f /etc/systemd/system/%sdocker.service 2>/dev/null; true", inst.Prefix))
	run(client, fmt.Sprintf("rm -f %s 2>/dev/null; true", inst.EnvFile))
	run(client, "sudo umount /var/tmp/btrfs-mount 2>/dev/null; true")
	run(client, "sudo rm -rf /var/tmp/btrfs-mount /var/tmp/btrfs-test.img 2>/dev/null; true")
	run(client, "sudo systemctl daemon-reload 2>/dev/null; true")
	run(client, fmt.Sprintf("sudo systemctl reset-failed %sdocker 2>/dev/null; true", inst.Prefix))

	// Upload dockyard.sh
	{
		start := time.Now()
		err := upload(client, "dist/dockyard.sh", "~/dockyard.sh")
		d := time.Since(start)
		if err != nil {
			fail(1, "Upload dockyard.sh", err.Error(), d)
			return
		}
		pass(1, "Upload dockyard.sh", d)
	}

	// gen-env
	{
		start := time.Now()
		cmd := fmt.Sprintf(
			"rm -f %s && DOCKYARD_ENV=%s DOCKYARD_ROOT=%s DOCKYARD_DOCKER_PREFIX=%s ~/dockyard.sh gen-env",
			inst.EnvFile, inst.EnvFile, inst.Root, inst.Prefix,
		)
		_, se, code := run(client, cmd)
		d := time.Since(start)
		if code != 0 {
			fail(2, "gen-env", se, d)
			return
		}
		pass(2, fmt.Sprintf("gen-env (%s / %s)", inst.Root, inst.Prefix), d)
	}

	// create
	{
		start := time.Now()
		_, se, code := run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh create", inst.EnvFile))
		d := time.Since(start)
		if code != 0 {
			fail(3, "create instance", se, d)
			return
		}
		pass(3, "create instance", d)
	}

	// Verify instance is healthy
	{
		start := time.Now()
		out, se, code := run(client, fmt.Sprintf("sudo %s run --rm alpine echo hello", inst.Docker))
		d := time.Since(start)
		if code != 0 || !strings.Contains(out, "hello") {
			fail(4, "instance health check", se, d)
			return
		}
		pass(4, "instance health check (container run)", d)
	}

	// BTRFS setup
	btrfsImg := "/var/tmp/btrfs-test.img"
	btrfsMnt := "/var/tmp/btrfs-mount"

	{
		_, _, mkfsCode := run(client, "which mkfs.btrfs")
		if mkfsCode != 0 {
			fmt.Println("[SKIP] mkfs.btrfs not found — install btrfs-progs")
			return
		}
	}

	{
		_, se, code := run(client, fmt.Sprintf(
			"sudo truncate -s 1G %s && sudo mkfs.btrfs -f %s && "+
				"sudo mkdir -p %s && sudo mount %s %s",
			btrfsImg, btrfsImg, btrfsMnt, btrfsImg, btrfsMnt,
		))
		if code != 0 {
			fmt.Printf("[SKIP] BTRFS setup failed: %s\n", strings.TrimSpace(se))
			return
		}
	}

	// 39 — BTRFS bind mount with runc (baseline)
	{
		start := time.Now()
		testDir := btrfsMnt + "/runc-test"
		run(client, fmt.Sprintf("sudo mkdir -p %s && sudo chmod 777 %s", testDir, testDir))

		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm --runtime=runc -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-runc-ok'",
			inst.Docker, testDir,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-runc-ok") {
			pass(40, "BTRFS bind mount with runc (baseline)", d)
		} else {
			fail(40, "BTRFS bind mount with runc (baseline)", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
		}
		run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
	}

	// 40 — BTRFS bind mount with sysbox-runc (the bug case)
	{
		start := time.Now()
		testDir := btrfsMnt + "/sysbox-test"
		run(client, fmt.Sprintf("sudo mkdir -p %s && sudo chmod 777 %s", testDir, testDir))

		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-sysbox-ok'",
			inst.Docker, testDir,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-sysbox-ok") {
			pass(41, "BTRFS bind mount with sysbox (chmod+touch on mountpoint)", d)
		} else {
			fail(41, "BTRFS bind mount with sysbox (chmod+touch on mountpoint)", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
		}
		run(client, fmt.Sprintf("sudo rm -rf %s", testDir))
	}

	// 41 — BTRFS subvolume bind mount with sysbox-runc
	{
		start := time.Now()
		subvol := btrfsMnt + "/subvol-test"
		run(client, fmt.Sprintf("sudo btrfs subvolume create %s 2>/dev/null || sudo mkdir -p %s", subvol, subvol))
		run(client, fmt.Sprintf("sudo chmod 777 %s", subvol))

		out, se, code := run(client, fmt.Sprintf(
			"sudo %s run --rm -v %s:/mnt alpine sh -c "+
				"'chmod 777 /mnt && touch /mnt/hello && echo btrfs-subvol-ok'",
			inst.Docker, subvol,
		))
		d := time.Since(start)
		if code == 0 && strings.Contains(out, "btrfs-subvol-ok") {
			pass(42, "BTRFS subvolume bind mount with sysbox", d)
		} else {
			fail(42, "BTRFS subvolume bind mount with sysbox", fmt.Sprintf("exit=%d stderr=%s", code, strings.TrimSpace(se)), d)
		}
		run(client, fmt.Sprintf("sudo btrfs subvolume delete %s 2>/dev/null; sudo rm -rf %s 2>/dev/null; true", subvol, subvol))
	}

	// Cleanup
	run(client, fmt.Sprintf("sudo umount %s 2>/dev/null; true", btrfsMnt))
	run(client, fmt.Sprintf("sudo rm -rf %s %s 2>/dev/null; true", btrfsMnt, btrfsImg))
	run(client, fmt.Sprintf("sudo env DOCKYARD_ENV=%s ~/dockyard.sh destroy --yes 2>/dev/null; true", inst.EnvFile))
}

// checkIsolation verifies daemon-level isolation: containers from one instance
// are not visible in another instance's docker ps. Returns failure messages.
func checkIsolation(client *ssh.Client, instances []Instance) []string {
	type cinfo struct {
		inst Instance
		name string
	}

	// Start a long-lived container in each instance with a unique name
	var containers []cinfo
	for _, inst := range instances {
		name := "iso-" + strings.ToLower(inst.Label) + "-check"
		run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", inst.Docker, name))
		_, _, code := run(client, fmt.Sprintf(
			"sudo %s run -d --name %s alpine sleep 60",
			inst.Docker, name,
		))
		if code == 0 {
			containers = append(containers, cinfo{inst, name})
		}
	}

	var fails []string
	for _, src := range containers {
		for _, viewer := range instances {
			if src.inst.Label == viewer.Label {
				continue
			}
			out, _, _ := run(client, fmt.Sprintf(
				"sudo %s ps -a --format '{{.Names}}'",
				viewer.Docker,
			))
			if strings.Contains(out, src.name) {
				fails = append(fails, fmt.Sprintf(
					"container %s (from %s) visible in %s's docker ps — daemon not isolated",
					src.name, src.inst.Label, viewer.Label,
				))
			}
		}
	}

	// Cleanup
	for _, c := range containers {
		run(client, fmt.Sprintf("sudo %s rm -f %s 2>/dev/null", c.inst.Docker, c.name))
	}
	return fails
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	if *hostFlag == "" || *userFlag == "" {
		fmt.Fprintf(os.Stderr, "Usage: dockyardtest --host HOST --user USER [--key PATH]\n")
		os.Exit(1)
	}

	kp := *keyFlag
	if kp == "" {
		home, _ := os.UserHomeDir()
		kp = filepath.Join(home, ".ssh", "id_ed25519")
	}

	fmt.Printf("Connecting to %s@%s:%d...\n", *userFlag, *hostFlag, *portFlag)
	client, err := dialSSH(*hostFlag, *portFlag, *userFlag, kp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH connect failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()
	fmt.Println("Connected.")

	_, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	suiteStart := time.Now()

	switch strings.ToLower(*onlyFlag) {
	case "btrfs":
		fmt.Println("[INFO] Running BTRFS-only test mode")
		runBtrfsOnly(client)
	case "":
		runTests(client, *hostFlag, *portFlag, *userFlag, kp)
	default:
		fmt.Fprintf(os.Stderr, "Unknown --only value: %s (supported: btrfs)\n", *onlyFlag)
		os.Exit(1)
	}

	totalElapsed := time.Since(suiteStart)

	total := len(results) // in focused mode, count only what ran
	if *onlyFlag == "" {
		total = 41 // full suite expected count
	}
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		}
	}
	skipped := total - len(results)
	fmt.Printf("\n=== Results: %d/%d passed", passed, total)
	if skipped > 0 {
		fmt.Printf(", %d skipped (earlier failure)", skipped)
	}
	fmt.Printf(" — total %s ===\n", fmtDur(totalElapsed))

	if passed < len(results) {
		os.Exit(1)
	}
}
