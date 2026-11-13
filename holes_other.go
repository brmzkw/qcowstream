//go:build !linux && !darwin

package qcowstream

const (
	seekData = -1 // unsupported: FileHoles reports no holes
	seekHole = -1
)
