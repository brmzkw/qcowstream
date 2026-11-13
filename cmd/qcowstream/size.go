package main

import (
	"fmt"
	"strconv"
	"strings"
)

var units = []struct {
	suffix string
	factor uint64
}{
	{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"kB", 1e3},
	{"B", 1},
}

// parseSize reads a byte count with an optional IEC (KiB, MiB...) or SI
// (KB, MB...) suffix. Bare K, M or G are refused: they are ambiguous.
func parseSize(s string) (uint64, error) {
	num, factor := s, uint64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			num, factor = strings.TrimSuffix(s, u.suffix), u.factor
			break
		}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: use a number of bytes with an optional KiB/MiB/GiB/TiB (IEC) or KB/MB/GB/TB (SI) suffix", s)
	}
	if n != 0 && factor > ^uint64(0)/n {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return n * factor, nil
}

// formatIEC renders bytes with binary units, saying so.
func formatIEC(n uint64) string {
	for _, u := range units[:5] {
		if n >= u.factor {
			return fmt.Sprintf("%.2f %s", float64(n)/float64(u.factor), u.suffix)
		}
	}
	return fmt.Sprintf("%d B", n)
}
