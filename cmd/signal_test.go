//go:build unix

package cmd

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets TestDeploy_SecondCtrlCLeavesNoRegistryLogin run this test
// binary as kx itself, with real signal handling: KX_TEST_RUN_MAIN=1 makes
// it kx.
func TestMain(m *testing.M) {
	if os.Getenv("KX_TEST_RUN_MAIN") == "1" {
		os.Exit(Main())
	}
	os.Exit(m.Run())
}

// slowPushDocker is a docker that builds nothing and whose push never
// finishes. Its push leaves a child process holding docker's output open,
// as a CLI plugin can, so kx is still inside the push after the first
// Ctrl-C has killed docker.
const slowPushDocker = `#!/bin/sh
case "$1" in
version) echo linux ;;
buildx) printf 'Name:   default\nDriver: docker\n' ;;
build|tag) ;;
push)
	sleep 30 &
	echo $! >"$KX_TEST_SLEEP_PID"
	printf '%s' "$DOCKER_CONFIG" >"$KX_TEST_PUSHING.tmp" && mv "$KX_TEST_PUSHING.tmp" "$KX_TEST_PUSHING"
	wait
	;;
*) echo "fake docker: unexpected $*" >&2; exit 2 ;;
esac
`

// A second Ctrl-C ends kx at once, before any deferred clean-up runs. The
// throwaway Docker login must be gone by then: kx removes it at the first
// Ctrl-C.
func TestDeploy_SecondCtrlCLeavesNoRegistryLogin(t *testing.T) {
	if signal.Ignored(os.Interrupt) {
		t.Skip("this process ignores Ctrl-C, and kx would inherit that")
	}
	srv := httptest.NewServer(newFakeAPI(t))
	defer srv.Close()

	root := t.TempDir()
	bin, tmp, project := filepath.Join(root, "bin"), filepath.Join(root, "tmp"), filepath.Join(root, "shop-api")
	for _, d := range []string{bin, tmp, project} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(slowPushDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushing, sleepPID := filepath.Join(root, "pushing"), filepath.Join(root, "sleep-pid")
	t.Cleanup(func() {
		if b, err := os.ReadFile(sleepPID); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	kx := exec.Command(exe, "deploy")
	kx.Dir = project
	kx.Env = []string{
		"KX_TEST_RUN_MAIN=1",
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + root,
		"TMPDIR=" + tmp,
		"KX_API_URL=" + srv.URL + "/api/v1",
		"KX_CONFIG_DIR=" + filepath.Join(root, "config"),
		"KX_TOKEN=" + tokDeploy,
		"DOCKER_HOST=unix://" + filepath.Join(root, "docker.sock"),
		"KX_TEST_PUSHING=" + pushing,
		"KX_TEST_SLEEP_PID=" + sleepPID,
	}
	var out, errOut bytes.Buffer
	kx.Stdout, kx.Stderr = &out, &errOut
	if err := kx.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- kx.Wait() }()
	t.Cleanup(func() { _ = kx.Process.Kill() })

	var login string
	for deadline := time.Now().Add(30 * time.Second); login == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pushing); err == nil {
			login = string(b)
		}
		select {
		case err := <-done:
			t.Fatalf("kx ended (%v) before the push started\nstdout:\n%s\nstderr:\n%s", err, &out, &errOut)
		default:
		}
	}
	if login == "" {
		t.Fatal("the push never started")
	}
	if filepath.Dir(login) != tmp {
		t.Fatalf("the Docker login is in %s, want it in TMPDIR %s", login, tmp)
	}
	if _, err := os.Stat(filepath.Join(login, "config.json")); err != nil {
		t.Fatalf("the Docker login isn't there during the push: %v", err)
	}

	// The first Ctrl-C, then the second: kx stops catching Ctrl-C once
	// the first arrives, so keep pressing until it ends.
	if err := kx.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(30 * time.Second)
	var waitErr error
wait:
	for {
		select {
		case waitErr = <-done:
			break wait
		case <-tick.C:
			_ = kx.Process.Signal(os.Interrupt)
		case <-timeout:
			t.Fatal("kx didn't end after a second Ctrl-C")
		}
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatalf("kx ended with %v, want it ended by the second Ctrl-C\nstderr:\n%s", waitErr, &errOut)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("kx ended on its own (%v), not at the second Ctrl-C\nstderr:\n%s", waitErr, &errOut)
	}

	if _, err := os.Stat(login); !os.IsNotExist(err) {
		t.Fatalf("the throwaway Docker login survived a second Ctrl-C (%v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "kx-docker-*")); len(left) != 0 {
		t.Fatalf("Docker logins left in TMPDIR: %v", left)
	}
}
