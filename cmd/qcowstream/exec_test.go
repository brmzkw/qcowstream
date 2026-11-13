package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brmzkw/qcowstream/internal/nbdtest"
	"github.com/brmzkw/qcowstream/internal/qcow2test"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds the command once per test run.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "qcowstream-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "qcowstream")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if err == nil {
		return 0
	}
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		t.Fatalf("killed by %v instead of exiting", ws.Signal())
	}
	return ee.ExitCode()
}

func TestNBDFullThroughBinary(t *testing.T) {
	disk := randomDisk(8*MiB, 6)
	srv := &nbdtest.Server{Export: "scsi0", Data: disk, MinBlock: 512, PrefBlock: 4096, MaxBlock: 1 << 20}
	sock := srv.Start(t)
	cmd := exec.Command(binary(t), "full", "--nbd-socket", sock, "--export", "scsi0",
		"--cluster-size", "64KiB", "--concurrency", "4", "--output", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	im, err := qcow2test.Check(stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.Materialize(nil), disk) {
		t.Fatal("content differs")
	}
	if v := srv.Violations(); len(v) > 0 {
		t.Fatal(v)
	}
}

func TestSignalStopsAndCleansUp(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			srv := &nbdtest.Server{Export: "scsi0", Data: make([]byte, 64*MiB)}
			sock := srv.Start(t)
			srv.Stall.Store(true)
			out := filepath.Join(t.TempDir(), "base.qcow2")
			cmd := exec.Command(binary(t), "full", "--nbd-socket", sock, "--export", "scsi0", "--output", out)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Wait until the output exists: the process is reading.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(out); err == nil {
					break
				}
				if time.Now().After(deadline) {
					cmd.Process.Kill()
					t.Fatalf("output never created: %s", stderr.String())
				}
			}
			start := time.Now()
			cmd.Process.Signal(sig)
			code := exitCode(t, cmd.Wait())
			if code != 128+int(sig) {
				t.Fatalf("exit %d, want %d: %s", code, 128+int(sig), stderr.String())
			}
			if time.Since(start) > forceExitAfter/2 {
				t.Fatal("stopping took too long: cancellation did not reach the NBD read")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial output left behind")
			}
		})
	}
}

func TestBrokenPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(path, randomDisk(16*MiB, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary(t), "full", "--source-file", path, "--output", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stdout, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	stdout.Close()
	if code := exitCode(t, cmd.Wait()); code != exitError {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "incomplete write") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}
