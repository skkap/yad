package runner

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// The two ways a Unix reports its load average, parsed here rather than beside
// each system call so both are tested on whichever machine runs the tests.

// parseProcLoadavg reads the first field of Linux's /proc/loadavg, which is
// the one-minute average as decimal text: "0.52 0.47 0.51 1/834 12345".
func parseProcLoadavg(s string) (float64, bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(s), " ")
	v, err := strconv.ParseFloat(first, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// darwinLoadScale is FSCALE from <sys/param.h> — 1 << FSHIFT, and FSHIFT is 11
// — the fixed point the kernel stores each average in. It is the fallback when
// the struct's own fscale does not survive the read: syscall.Sysctl trims a
// single trailing NUL byte, and on every Mac measured fscale is 2048, whose
// little-endian high byte is that NUL. So the 24-byte struct arrives as 23
// bytes with the last byte of the divisor gone.
const darwinLoadScale = 2048

// parseSysctlLoadavg reads darwin's vm.loadavg, a struct loadavg — three
// fixpt_t averages, four bytes of padding, then a long fscale to divide by.
// Short input is padded rather than refused, for the trimmed NUL above.
func parseSysctlLoadavg(b []byte) (float64, bool) {
	if len(b) < 4 {
		return 0, false
	}
	scale := float64(darwinLoadScale)
	if len(b) > 16 {
		tail := make([]byte, 8)
		copy(tail, b[16:])
		if n := binary.LittleEndian.Uint64(tail); n > 0 {
			scale = float64(n)
		}
	}
	return float64(binary.LittleEndian.Uint32(b[:4])) / scale, true
}
