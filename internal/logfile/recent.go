package logfile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Record is one warning or error kept for `yad status`.
type Record struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   string // key=value pairs, as a text handler writes them
}

// Recent keeps the last warnings and errors in memory, so `yad status` can
// show what went wrong without anyone reading the log.
type Recent struct {
	mu   sync.Mutex
	keep int
	recs []Record
}

// NewRecent keeps the last n records at slog.LevelWarn or above.
func NewRecent(n int) *Recent { return &Recent{keep: n} }

// Records returns what is kept, oldest first.
func (r *Recent) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.recs...)
}

func (r *Recent) add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	if len(r.recs) > r.keep {
		r.recs = r.recs[len(r.recs)-r.keep:]
	}
}

// Handler returns next wrapped so that every record it handles at warning or
// above is kept here too.
func (r *Recent) Handler(next slog.Handler) slog.Handler {
	return &recentHandler{r: r, next: next}
}

type recentHandler struct {
	r      *Recent
	next   slog.Handler
	prefix []slog.Attr // from WithAttrs; groups are not used by the daemon
}

func (h *recentHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.next.Enabled(ctx, l)
}

func (h *recentHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		var b strings.Builder
		write := func(a slog.Attr) bool {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(a.String())
			return true
		}
		for _, a := range h.prefix {
			write(a)
		}
		rec.Attrs(write)
		h.r.add(Record{Time: rec.Time, Level: rec.Level, Message: truncate(rec.Message), Attrs: truncate(b.String())})
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

// maxRecordText bounds what one kept record holds. An error can carry a hub's
// whole response body; the file has it all, and status needs only enough to
// recognise the error by.
const maxRecordText = 2 << 10

func truncate(s string) string {
	if len(s) <= maxRecordText {
		return s
	}
	cut := maxRecordText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated; the log has it all)"
}

func (h *recentHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &recentHandler{r: h.r, next: h.next.WithAttrs(as), prefix: append(append([]slog.Attr(nil), h.prefix...), as...)}
}

func (h *recentHandler) WithGroup(name string) slog.Handler {
	return &recentHandler{r: h.r, next: h.next.WithGroup(name), prefix: h.prefix}
}
