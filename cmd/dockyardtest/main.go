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
	userFlag    = flag.String("user", "", "SSH username (required)")
	keyFlag     = flag.String("key", "", "Path to SSH private key (default: ~/.ssh/id_ed25519)")
	timeoutFlag = flag.Duration("timeout", 20*time.Minute, "Overall test timeout")
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
func dialSSH(host, user, keyPath string) (*ssh.Client, error) {
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
	client, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
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

// waitForSSH polls port 22 until reachable or timeout.
func waitForSSH(host string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", host+":22", 5*time.Second)
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
		run(client, fmt.Sprintf("sudo rm -rf %s 2>/dev/null; true", inst.Root))
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
	run(client, "sudo systemctl daemon-reload 2>/dev/null; true")
}

func runTests(client *ssh.Client, host, user, keyPath string) {
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
			_, se, code := run(c, fmt.Sprintf("sudo %s run -d --name %s docker:26.1-dind", inst.Docker, cname))
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
		if err := waitForSSH(host, 4*time.Minute); err != nil {
			fail(22, "reboot", err.Error(), time.Since(start))
			return
		}
		// Give systemd a few seconds to finish starting services
		time.Sleep(10 * time.Second)

		var reconnErr error
		client, reconnErr = dialSSH(host, user, keyPath)
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

			_, se, code := run(c, fmt.Sprintf("sudo %s run -d --name %s docker:26.1-dind", inst.Docker, cname))
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
			// data directory (instance root)
			out, _, _ := run(client, fmt.Sprintf("[ -d %s ] && echo exists || echo gone", inst.Root))
			if strings.TrimSpace(out) == "exists" {
				cleanFails = append(cleanFails, inst.Label+": "+inst.Root+" still exists")
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

	fmt.Printf("Connecting to %s@%s...\n", *userFlag, *hostFlag)
	client, err := dialSSH(*hostFlag, *userFlag, kp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SSH connect failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()
	fmt.Println("Connected.")

	_, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	suiteStart := time.Now()
	runTests(client, *hostFlag, *userFlag, kp)
	totalElapsed := time.Since(suiteStart)

	total := 30 // total expected tests
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
