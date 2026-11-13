package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// qemuEnv is a running container with qemu-utils.
type qemuEnv struct {
	Name    string // base image
	Version string // qemu-img --version, first line
	c       testcontainers.Container
}

// Base images of the compatibility matrix. Each brings a different QEMU.
var bases = []string{"ubuntu:24.04", "debian:trixie"}

var envs []*qemuEnv

func TestMain(m *testing.M) {
	ctx := context.Background()
	var code int
	defer func() {
		for _, e := range envs {
			_ = e.c.Terminate(ctx)
		}
		os.Exit(code)
	}()
	for _, base := range bases {
		e, err := startQemu(ctx, base)
		if err != nil {
			// Integration tests are mandatory: no Docker is a failure, not a skip.
			fmt.Fprintf(os.Stderr, "integration: cannot start %s: %v\n", base, err)
			code = 1
			return
		}
		fmt.Fprintf(os.Stderr, "integration: %s: %s\n", base, e.Version)
		envs = append(envs, e)
	}
	code = m.Run()
}

func startQemu(ctx context.Context, base string) (*qemuEnv, error) {
	tag := strings.NewReplacer(":", "-", ".", "-").Replace(base)
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    "testdata",
				Dockerfile: "Dockerfile",
				BuildArgs:  map[string]*string{"BASE": &base},
				Repo:       "qcowstream-qemu",
				Tag:        tag,
				KeepImage:  true,
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}
	e := &qemuEnv{Name: base, c: c}
	out, err := e.exec(ctx, "qemu-img", "--version")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}
	e.Version, _, _ = strings.Cut(out, "\n")
	return e, nil
}

func (e *qemuEnv) exec(ctx context.Context, cmd ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	code, r, err := e.c.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", err
	}
	out, _ := io.ReadAll(r)
	if code != 0 {
		return string(out), fmt.Errorf("%s: exit %d: %s", strings.Join(cmd, " "), code, out)
	}
	return string(out), nil
}

// run executes a shell command and fails the test on a non-zero exit.
func (e *qemuEnv) run(t *testing.T, script string) string {
	t.Helper()
	out, err := e.exec(context.Background(), "sh", "-ec", script)
	if err != nil {
		t.Fatalf("%s: %v", e.Name, err)
	}
	return out
}

// fails executes a shell command that must fail, and returns its output.
func (e *qemuEnv) fails(t *testing.T, script string) string {
	t.Helper()
	out, err := e.exec(context.Background(), "sh", "-ec", script)
	if err == nil {
		t.Fatalf("%s: %q succeeded: %s", e.Name, script, out)
	}
	return out
}

func (e *qemuEnv) put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := e.c.CopyToContainer(context.Background(), data, path, 0o644); err != nil {
		t.Fatalf("%s: copy %s: %v", e.Name, path, err)
	}
}

func (e *qemuEnv) get(t *testing.T, path string) []byte {
	t.Helper()
	r, err := e.c.CopyFileFromContainer(context.Background(), path)
	if err != nil {
		t.Fatalf("%s: copy %s: %v", e.Name, path, err)
	}
	defer r.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// dir creates a fresh working directory in the container.
func (e *qemuEnv) dir(t *testing.T) string {
	t.Helper()
	d := "/work/" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	e.run(t, "rm -rf "+d+" && mkdir -p "+d)
	return d
}

// each runs f against every QEMU environment.
func each(t *testing.T, f func(t *testing.T, e *qemuEnv)) {
	for _, e := range envs {
		t.Run(e.Name, func(t *testing.T) { f(t, e) })
	}
}
