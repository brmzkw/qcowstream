package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	cliOnce sync.Map // arch -> *cliBuild
)

type cliBuild struct {
	once sync.Once
	data []byte
	err  error
}

// installCLI cross-compiles qcowstream for the container's architecture and
// copies it to /usr/local/bin.
func (e *qemuEnv) installCLI(t *testing.T) {
	t.Helper()
	machine := strings.TrimSpace(e.run(t, "uname -m"))
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[machine]
	if arch == "" {
		t.Fatalf("unsupported container architecture %q", machine)
	}
	v, _ := cliOnce.LoadOrStore(arch, &cliBuild{})
	b := v.(*cliBuild)
	b.once.Do(func() {
		dir, err := os.MkdirTemp("", "qcowstream-linux")
		if err != nil {
			b.err = err
			return
		}
		out := filepath.Join(dir, "qcowstream")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/qcowstream")
		cmd.Dir = ".." // the root module, with its own go.sum
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
		if msg, err := cmd.CombinedOutput(); err != nil {
			b.err = &buildError{string(msg)}
			return
		}
		b.data, b.err = os.ReadFile(out)
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	if err := e.c.CopyToContainer(context.Background(), b.data, "/usr/local/bin/qcowstream", 0o755); err != nil {
		t.Fatal(err)
	}
}

type buildError struct{ out string }

func (e *buildError) Error() string { return "build qcowstream: " + e.out }
