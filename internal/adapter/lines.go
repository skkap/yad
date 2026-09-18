package adapter

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// MaxLine caps one line of a harness's stream. Multica's 10 MiB cap made a long
// thread unresumable forever — every resume replayed the oversized line and
// died on it — so the cap is generous, and a line over it is skipped rather than
// fatal.
const MaxLine = 32 << 20

// ErrLineTooLong reports a line over the cap. The line has been consumed and the
// reader is positioned at the next one; the caller reports it and carries on.
type ErrLineTooLong struct{ Size int }

func (e *ErrLineTooLong) Error() string {
	return fmt.Sprintf("a %d-byte line exceeded the %d MiB cap and was skipped", e.Size, MaxLine>>20)
}

// LineReader reads newline-delimited frames with a cap on each. bufio.Scanner
// is unsuitable: after one oversized token it stops for good, which turns a
// single huge tool result into a dead run.
type LineReader struct {
	r   *bufio.Reader
	max int
}

// NewLineReader reads r with the MaxLine cap.
func NewLineReader(r io.Reader) *LineReader { return newLineReader(r, MaxLine) }

func newLineReader(r io.Reader, max int) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next line without its newline. A final line with no newline
// is returned before io.EOF. The slice is only valid until the next call.
func (l *LineReader) Next() ([]byte, error) {
	var line []byte
	size := 0
	for {
		chunk, err := l.r.ReadSlice('\n')
		size += len(chunk)
		if size <= l.max+1 { // +1: the newline does not count against the cap
			line = append(line, chunk...)
		} else {
			line = nil
		}
		switch {
		case err == nil:
			if size > l.max+1 {
				return nil, &ErrLineTooLong{Size: size - 1}
			}
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && size > 0:
			if size > l.max {
				return nil, &ErrLineTooLong{Size: size}
			}
			return line, nil
		default:
			return nil, err
		}
	}
}
