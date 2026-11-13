// Command qcowstream writes full and incremental QCOW2 images as a
// sequential stream, from a raw file or an NBD export.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// forceExitAfter bounds how long the process waits for a clean stop after a
// signal. A write blocked on a stalled pipe cannot see the cancellation.
const forceExitAfter = 10 * time.Second

func main() {
	// With SIGPIPE delivered to a channel, a write to a closed stdout returns
	// EPIPE: the error path runs and the exit status is non-zero, instead of
	// the runtime killing the process.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	var code atomic.Int32
	go func() {
		sig := <-sigs
		code.Store(128 + int32(sig.(syscall.Signal)))
		fmt.Fprintf(os.Stderr, "qcowstream: %v, stopping; the output is incomplete\n", sig)
		cancel()
		select {
		case <-sigs:
		case <-time.After(forceExitAfter):
		}
		os.Exit(int(code.Load()))
	}()

	c := &cli{stdout: os.Stdout, stderr: os.Stderr, openFile: openOSFile}
	rc := c.run(ctx, os.Args[1:])
	if ctx.Err() != nil {
		rc = int(code.Load())
	}
	os.Exit(rc)
}
