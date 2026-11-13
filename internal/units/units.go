// Package units parses and formats byte counts with explicit IEC or SI
// units.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

// Binary sizes.
const (
	KiB = uint64(1) << 10
	MiB = KiB << 10
	GiB = MiB << 10
	TiB = GiB << 10
	PiB = TiB << 10
)

var units = []struct {
	suffix string
	factor uint64
}{
	{"PiB", PiB}, {"TiB", TiB}, {"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB},
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"kB", 1e3},
	{"B", 1},
}

// Parse reads a byte count with an optional IEC (KiB, MiB...) or SI
// (KB, MB...) suffix. Bare K, M or G are refused: they are ambiguous.
func Parse(s string) (uint64, error) {
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

// IEC renders bytes with binary units, saying so.
func IEC(n uint64) string {
	for _, u := range units[:5] {
		if n >= u.factor {
			return fmt.Sprintf("%.2f %s", float64(n)/float64(u.factor), u.suffix)
		}
	}
	return fmt.Sprintf("%d B", n)
}
