package adapter

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLineReader(t *testing.T) {
	long := strings.Repeat("x", 100)
	cases := []struct {
		name  string
		in    string
		max   int
		want  []string // "!" marks an ErrLineTooLong at that position
		wantN []int    // sizes reported for each "!"
	}{
		{"lines", "a\nbb\n", 10, []string{"a", "bb"}, nil},
		{"no final newline", "a\nbb", 10, []string{"a", "bb"}, nil},
		{"empty lines kept", "\n\na\n", 10, []string{"", "", "a"}, nil},
		{"exactly the cap", "0123456789\n", 10, []string{"0123456789"}, nil},
		{"over the cap is skipped, not fatal", "a\n" + long + "\nb\n", 10, []string{"a", "!", "b"}, []int{100}},
		{"over the cap at EOF", "a\n" + long, 10, []string{"a", "!"}, []int{100}},
		// Longer than bufio's buffer, so ReadSlice fills it several times.
		{"a long line under the cap", strings.Repeat("y", 200<<10) + "\nz\n", 300 << 10, []string{strings.Repeat("y", 200<<10), "z"}, nil},
		{"a long line over the cap", strings.Repeat("y", 200<<10) + "\nz\n", 100 << 10, []string{"!", "z"}, []int{200 << 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lr := newLineReader(strings.NewReader(c.in), c.max)
			var got []string
			var sizes []int
			for {
				line, err := lr.Next()
				var tooLong *ErrLineTooLong
				if errors.As(err, &tooLong) {
					got = append(got, "!")
					sizes = append(sizes, tooLong.Size)
					continue
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, string(line))
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("got %q, want %q", short(got), short(c.want))
			}
			if len(sizes) != len(c.wantN) || (len(sizes) > 0 && sizes[0] != c.wantN[0]) {
				t.Errorf("sizes %v, want %v", sizes, c.wantN)
			}
		})
	}
}

func short(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		if len(x) > 20 {
			x = x[:20] + "…"
		}
		out[i] = x
	}
	return out
}

func TestMaxLineIs32MiB(t *testing.T) {
	if MaxLine != 32<<20 {
		t.Errorf("MaxLine = %d; ARCHITECTURE.md §3 promises 32 MiB", MaxLine)
	}
}
