package runner

import (
	"math"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestParseProcLoadavg(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{name: "a busy machine", in: "5.16 4.75 4.14 3/834 12345\n", want: 5.16, ok: true},
		{name: "an idle one", in: "0.00 0.01 0.05 1/210 99\n", want: 0, ok: true},
		{name: "nothing there", in: "", want: 0},
		{name: "not a number", in: "n/a 0.01 0.05\n", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcLoadavg(tc.in)
			if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("parseProcLoadavg(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseSysctlLoadavg(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want float64
		ok   bool
	}{
		{
			// Measured: these are the exact bytes syscall.Sysctl("vm.loadavg")
			// returned on a macOS 25.6 machine whose uptime(1) said
			// "load averages: 5.16 4.75 4.14" in the same second. Twenty-three
			// bytes, not twenty-four — Sysctl trims one trailing NUL, and
			// fscale's high byte is a NUL, so the divisor arrives a byte short.
			name: "a real darwin read, whose last byte was trimmed",
			in:   []byte{71, 41, 0, 0, 4, 38, 0, 0, 25, 33, 0, 0, 0, 0, 0, 0, 0, 8, 0, 0, 0, 0, 0},
			want: 10567.0 / 2048.0,
			ok:   true,
		},
		{
			name: "the whole struct, when nothing is trimmed",
			in:   []byte{71, 41, 0, 0, 4, 38, 0, 0, 25, 33, 0, 0, 0, 0, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0},
			want: 10567.0 / 2048.0,
			ok:   true,
		},
		{
			// A kernel reporting a different fixed point is believed over the
			// constant; that is what the field is for.
			name: "a scale of its own",
			in:   []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0},
			want: 1,
			ok:   true,
		},
		{name: "too short to hold an average", in: []byte{1, 2, 3}},
		{name: "nothing at all", in: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSysctlLoadavg(tc.in)
			if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("parseSysctlLoadavg(%v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Whatever this machine is, what health carries has to be a number a hub can
// route on. A platform yad does not ship for reports 0, which is why this
// asserts a floor rather than a load.
func TestLoadAverageIsANumber(t *testing.T) {
	got := loadAverage()
	if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
		t.Errorf("loadAverage() = %v", got)
	}
}

// Measured, not assumed: the decode above is read as a struct with a layout
// and a fixed point this code asserts, so on the development machine it is
// checked against the number uptime(1) prints from the same kernel field.
// Skipped where uptime is not there to ask, and generous about the gap: the
// two reads are seconds apart and the average moves between them.
func TestLoadAverageAgreesWithUptime(t *testing.T) {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		t.Skipf("no uptime to compare against: %v", err)
	}
	_, after, ok := strings.Cut(string(out), "load average")
	if !ok {
		t.Skipf("uptime said nothing about load: %q", out)
	}
	fields := strings.FieldsFunc(strings.TrimLeft(after, "s: "), func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		t.Skipf("uptime said nothing about load: %q", out)
	}
	want, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Skipf("uptime's load average did not parse: %q", fields[0])
	}
	got := loadAverage()
	if math.Abs(got-want) > 1+want/2 {
		t.Errorf("loadAverage() = %v, uptime says %v — the decode disagrees with the kernel by more than the averages drift", got, want)
	}
	t.Logf("loadAverage() = %v, uptime says %v", got, want)
}
