package units

import "testing"

func TestParse(t *testing.T) {
	good := map[string]uint64{"4096": 4096, "64KiB": 64 << 10, "64KB": 64000, "64kB": 64000, "2MiB": 2 << 20,
		"128MiB": 128 << 20, "1GiB": 1 << 30, "1GB": 1e9, "100TiB": 100 << 40, "1TB": 1e12, "512B": 512}
	for s, want := range good {
		if got, err := Parse(s); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", s, got, err, want)
		}
	}
	for _, s := range []string{"", "64K", "1M", "1G", "1.5GiB", "-1", "KiB", "99999999999PiB"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestIEC(t *testing.T) {
	for n, want := range map[uint64]string{512: "512 B", 64 * KiB: "64.00 KiB", 1536 * MiB: "1.50 GiB", 100 * TiB: "100.00 TiB"} {
		if got := IEC(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}
