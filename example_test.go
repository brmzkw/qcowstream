package qcowstream_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/brmzkw/qcowstream"
	"github.com/brmzkw/qcowstream/nbdsource"
)

// A full image of a raw disk, streamed to any io.Writer.
func Example_full() {
	disk := make([]byte, 8<<20) // stands in for an *os.File
	src := qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk)))

	ext := qcowstream.Extents{}
	plan, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode:        qcowstream.Full,
		VirtualSize: src.Size(),
		ClusterSize: 64 << 10,
	})
	if err != nil {
		log.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := qcowstream.Write(context.Background(), &out, src, ext, plan); err != nil {
		log.Fatal(err) // the stream is partial: discard it
	}
	fmt.Println(uint64(out.Len()) == plan.Size(), plan.Size())
	// Output: true 8716288
}

// An incremental image holding the changed regions. Its parent, base.qcow2,
// is never opened: only its name is recorded.
func Example_incremental() {
	disk := make([]byte, 64<<20)
	src := qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk)))

	ext := qcowstream.Extents{Changed: qcowstream.SliceExtents{
		{Offset: 4 << 20, Length: 4 << 20},
		{Offset: 32 << 20, Length: 8 << 20},
	}}
	plan, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode:        qcowstream.Incremental,
		VirtualSize: src.Size(),
		BackingFile: "base.qcow2",
	})
	if err != nil {
		log.Fatal(err)
	}
	info := plan.Info()
	fmt.Println(info.ClusterSize, info.DataClusters, info.SourceBytes)
	if _, err := qcowstream.Write(context.Background(), io.Discard, src, ext, plan); err != nil {
		log.Fatal(err)
	}
	// Output: 2097152 6 12582912
}

// Streaming through a pipe: the consumer, such as an uploader, reads while
// the image is produced. Nothing seeks and nothing is staged.
func Example_pipe() {
	disk := make([]byte, 4<<20)
	src := qcowstream.NewReaderAtSource(bytes.NewReader(disk), uint64(len(disk)))
	ext := qcowstream.Extents{}
	plan, err := qcowstream.Plan(context.Background(), ext, qcowstream.Options{
		Mode: qcowstream.Full, VirtualSize: src.Size()})
	if err != nil {
		log.Fatal(err)
	}

	r, w := io.Pipe()
	go func() {
		_, err := qcowstream.Write(context.Background(), w, src, ext, plan)
		w.CloseWithError(err) // a failure reaches the reader, never a short success
	}()
	n, err := io.Copy(io.Discard, r)
	fmt.Println(n == int64(plan.Size()), err)
	// Output: true <nil>
}

// An incremental image from a QEMU dirty bitmap exported over NBD, as a
// Proxmox backup provider exposes it. Zeros reported by base:allocation are
// not read.
func Example_nbd() {
	ctx := context.Background()
	src, err := nbdsource.Open(ctx, nbdsource.Options{
		Socket:      "/run/qemu-server/100-nbd.backup-access",
		Export:      "drive-scsi0",
		Bitmap:      "snapshot-access:plakar",
		DetectZeros: true,
		Connections: 4,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	ext := qcowstream.Extents{Changed: src.Changed(), Zeros: src.Zeros()}
	plan, err := qcowstream.Plan(ctx, ext, qcowstream.Options{
		Mode:        qcowstream.Incremental,
		VirtualSize: src.Size(),
		BackingFile: "inc-1.qcow2",
		Concurrency: 4,
	})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := qcowstream.Write(ctx, os.Stdout, src, ext, plan); err != nil {
		log.Fatal(err)
	}
}
