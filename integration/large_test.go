package integration

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/internal/virtual"
)

const (
	GiB = MiB << 10
	TiB = GiB << 10
)

// TestIncrementOn100TiB checks with QEMU an increment of a 100 TiB virtual
// disk, without any 100 TiB transfer: qemu-img check with the backing chain
// disabled, then qemu-io reads of the written regions and around them.
func TestIncrementOn100TiB(t *testing.T) {
	disk := virtual.Disk{Len: 100 * TiB}
	changed := virtual.Strided{Start: 1 * GiB, Stride: 1 * TiB, Length: 64 * KiB, Count: 100}
	ext := qcowstream.Extents{Changed: changed}
	p, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Incremental, VirtualSize: disk.Len, ClusterSize: 64 * KiB, BackingFile: "base.qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	var img bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &img, disk, ext, p); err != nil {
		t.Fatal(err)
	}

	var cmds []string
	for i := range uint64(100) {
		off := 1*GiB + i*TiB
		cmds = append(cmds,
			fmt.Sprintf("-c 'read -P %d %d 64k'", virtual.Pattern(off), off),
			fmt.Sprintf("-c 'read -P 0 %d 64k'", off+64*KiB), // unallocated, no backing: zeros
		)
	}
	each(t, func(t *testing.T, e *qemuEnv) {
		dir := e.dir(t)
		e.put(t, dir+"/inc.qcow2", img.Bytes())
		const alone = `'json:{"driver":"qcow2","backing":null,"file":{"driver":"file","filename":"inc.qcow2"}}'`
		e.run(t, "cd "+dir+" && qemu-img check "+alone)
		info := e.run(t, "cd "+dir+" && qemu-img info "+alone)
		if !strings.Contains(info, "100 TiB") {
			t.Fatalf("virtual size:\n%s", info)
		}
		out := e.run(t, "cd "+dir+" && qemu-io -r "+strings.Join(cmds, " ")+" "+alone)
		if strings.Contains(out, "Pattern verification failed") || strings.Count(out, "read 65536/65536 bytes") != 200 {
			t.Fatalf("qemu-io:\n%s", out)
		}
	})
}
