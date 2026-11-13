package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/brmzkw/qcowstream"
)

const (
	KiB = uint64(1) << 10
	MiB = KiB << 10
)

func randomDisk(size, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, ^seed))
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func generateFull(t *testing.T, w io.Writer, disk []byte, cs uint64) error {
	t.Helper()
	ext := qcowstream.Extents{}
	p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: uint64(len(disk)), ClusterSize: cs})
	if err != nil {
		t.Fatal(err)
	}
	_, err = qcowstream.Write(context.Background(), w, qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk))), ext, p)
	return err
}

func fullImage(t *testing.T, disk []byte, cs uint64) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := generateFull(t, &b, disk, cs); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// checkImage runs qemu-img check, converts the image to raw and compares it
// byte for byte with want.
func checkImage(t *testing.T, e *qemuEnv, dir, image string, want []byte) {
	t.Helper()
	e.put(t, dir+"/want.raw", want)
	e.run(t, fmt.Sprintf("cd %s && qemu-img check -f qcow2 %s && qemu-img convert -f qcow2 -O raw %s got.raw && cmp got.raw want.raw && rm got.raw want.raw",
		dir, image, image))
}

func TestFullImage(t *testing.T) {
	disk := randomDisk(5*MiB+512, 1)
	each(t, func(t *testing.T, e *qemuEnv) {
		for _, cs := range []uint64{512, 64 * KiB, 2 * MiB} {
			dir := e.dir(t)
			e.put(t, dir+"/full.qcow2", fullImage(t, disk, cs))
			checkImage(t, e, dir, "full.qcow2", disk)
		}
	})
}

func TestZeroFullImage(t *testing.T) {
	disk := make([]byte, 3*MiB)
	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/full.qcow2", fullImage(t, disk, 64*KiB))
		checkImage(t, e, dir, "full.qcow2", disk)
	})
}

// TestFullImageThroughPipe streams the image into a separate process through
// a pipe, which cannot seek, and checks the result is the same image.
func TestFullImageThroughPipe(t *testing.T) {
	disk := randomDisk(4*MiB+512, 2)
	want := fullImage(t, disk, 64*KiB)

	out := filepath.Join(t.TempDir(), "piped.qcow2")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("cat")
	cmd.Stdout = f
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := generateFull(t, stdin, disk, 64*KiB); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatal("piped image differs from the in-memory image")
	}
	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/piped.qcow2", got)
		checkImage(t, e, dir, "piped.qcow2", disk)
	})
}

// TestFullImageReaderGoesAway closes the reading process mid-stream: the
// writer must fail, not report success.
func TestFullImageReaderGoesAway(t *testing.T) {
	disk := randomDisk(8*MiB, 3)
	cmd := exec.Command("head", "-c", "100000")
	cmd.Stdout = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err = generateFull(t, stdin, disk, 64*KiB)
	stdin.Close()
	_ = cmd.Wait()
	if !errors.Is(err, qcowstream.ErrIncompleteWrite) {
		t.Fatalf("got %v", err)
	}
}
