// Package progress reports per-repo work while it runs. On a terminal it
// redraws a block of the repos currently being worked on, in place, under a
// running count; anywhere else it writes one plain line per event, so a log
// file or CI output never carries escape codes.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

const tick = 100 * time.Millisecond

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Tracker is safe for concurrent use by any number of workers.
//
// persist keeps a line for every finished item. It is off by default
// because a workspace sync routinely covers more repos than a terminal has
// rows: the live block is what shows activity, and the run's own summary is
// what is left behind.
type Tracker struct {
	mu      sync.Mutex
	w       io.Writer
	live    bool
	persist bool
	total   int
	started int
	done    int
	failed  int
	active  []*Item
	pending []string // persisted lines held back while paused
	drawn   int      // rows the live block currently occupies
	paused  int
	frame   int
	begin   time.Time
	now     func() time.Time
	size    func() (width, height int)
	stop    chan struct{}
	exited  chan struct{}
}

// Item is one unit of work, from Tracker.Start to Item.Done.
type Item struct {
	label string
	verb  string
	start time.Time
	t     *Tracker
}

// New returns a Tracker for total items. It draws live only when w is a
// terminal, and the caller must call Stop before writing anything else to w.
func New(w io.Writer, total int, persist bool) *Tracker {
	f, ok := w.(*os.File)
	live := ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
	size := func() (int, int) {
		width, height, err := term.GetSize(int(f.Fd()))
		if err != nil {
			return 80, 24
		}
		return width, height
	}
	t := newTracker(w, total, persist, live, time.Now, size)
	if live {
		go t.loop()
	}
	return t
}

func newTracker(w io.Writer, total int, persist, live bool, now func() time.Time, size func() (int, int)) *Tracker {
	return &Tracker{
		w:       w,
		live:    live,
		persist: persist,
		total:   total,
		begin:   now(),
		now:     now,
		size:    size,
		stop:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

// Start records label (an owner/repo) as now doing verb.
func (t *Tracker) Start(label, verb string) *Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	it := &Item{label: label, verb: verb, start: t.now(), t: t}
	t.started++
	if t.live {
		t.active = append(t.active, it)
	} else {
		fmt.Fprintf(t.w, "[%d/%d] %s: %s\n", t.started, t.total, label, verb)
	}
	return it
}

// Done ends the item; a non-nil err counts it as failed. The error itself
// is the caller's to report.
func (it *Item) Done(err error) {
	t := it.t
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done++
	if err != nil {
		t.failed++
	}
	for i, a := range t.active {
		if a == it {
			t.active = append(t.active[:i], t.active[i+1:]...)
			break
		}
	}
	if !t.persist {
		return
	}
	elapsed := formatElapsed(t.now().Sub(it.start))
	if !t.live {
		outcome := "done in"
		if err != nil {
			outcome = "failed after"
		}
		fmt.Fprintf(t.w, "[%d/%d] %s: %s %s %s\n", t.done, t.total, it.label, it.verb, outcome, elapsed)
		return
	}
	line := fmt.Sprintf("✓ %s %s %s", it.label, it.verb, elapsed)
	if err != nil {
		line = fmt.Sprintf("✗ %s %s failed after %s", it.label, it.verb, elapsed)
	}
	t.pending = append(t.pending, line)
	if t.paused == 0 {
		t.redraw()
	}
}

// Pause clears the live block and holds it off until the returned func is
// called, so something else (a confirmation prompt) can use the terminal.
// Pauses nest.
func (t *Tracker) Pause() (resume func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.live {
		return func() {}
	}
	t.paused++
	t.clear()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.paused--
			if t.paused == 0 {
				t.redraw()
			}
		})
	}
}

// Stop ends the live display and erases its block, leaving only persisted
// lines on screen.
func (t *Tracker) Stop() {
	if !t.live {
		return
	}
	close(t.stop)
	<-t.exited
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = 0
	t.redraw()
	t.clear()
}

func (t *Tracker) loop() {
	defer close(t.exited)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			t.mu.Lock()
			t.frame++
			if t.paused == 0 {
				t.redraw()
			}
			t.mu.Unlock()
		}
	}
}

// redraw rewrites the block in a single Write, flushing any pending
// persisted lines above it first so they scroll up out of the way. Each
// line is cut to the terminal's width: a wrapped line would take two rows
// and the next redraw would move up one too few.
func (t *Tracker) redraw() {
	width, height := t.size()
	var b strings.Builder
	t.moveToTop(&b)
	for _, line := range t.pending {
		b.WriteString(fit(line, width))
		b.WriteString("\x1b[K\n")
	}
	t.pending = nil
	lines := t.block(height)
	for _, line := range lines {
		b.WriteString(fit(line, width))
		b.WriteString("\x1b[K\n")
	}
	b.WriteString("\x1b[J")
	t.drawn = len(lines)
	io.WriteString(t.w, b.String())
}

func (t *Tracker) clear() {
	var b strings.Builder
	t.moveToTop(&b)
	b.WriteString("\x1b[J")
	t.drawn = 0
	io.WriteString(t.w, b.String())
}

// moveToTop is guarded because "\x1b[0F" moves up one row in most
// terminals, not zero.
func (t *Tracker) moveToTop(b *strings.Builder) {
	if t.drawn > 0 {
		fmt.Fprintf(b, "\x1b[%dF", t.drawn)
	}
}

// block is one row per active item, oldest first, then a count of the
// whole run. A single-item run has no count: "[0/1] 1 running" says
// nothing its one row doesn't. Rows beyond the terminal's height are
// folded into a "+N more" row so the block never scrolls, which would
// leave rows behind that the next redraw cannot reach.
func (t *Tracker) block(height int) []string {
	if len(t.active) == 0 && (t.started == 0 || t.total <= 1) {
		return nil
	}
	shown := t.active
	hidden := 0
	if room := height - 2; room >= 1 && len(shown) > room {
		hidden = len(shown) - (room - 1)
		shown = shown[:room-1]
	}

	labelWidth, verbWidth := 0, 0
	for _, it := range shown {
		labelWidth = max(labelWidth, len(it.label))
		verbWidth = max(verbWidth, len(it.verb))
	}
	now := t.now()
	spin := spinner[t.frame%len(spinner)]
	lines := make([]string, 0, len(shown)+2)
	for _, it := range shown {
		lines = append(lines, fmt.Sprintf("%s %-*s  %-*s  %s", spin, labelWidth, it.label, verbWidth, it.verb, formatElapsed(now.Sub(it.start))))
	}
	if hidden > 0 {
		lines = append(lines, fmt.Sprintf("  +%d more", hidden))
	}
	if t.total > 1 {
		summary := fmt.Sprintf("[%d/%d] %d running", t.done, t.total, len(t.active))
		if t.failed > 0 {
			summary += fmt.Sprintf(", %d failed", t.failed)
		}
		lines = append(lines, summary+"  "+formatElapsed(now.Sub(t.begin)))
	}
	return lines
}

// fit cuts s to one column short of width, so writing it never puts the
// cursor past the last column (where some terminals wrap immediately).
func fit(s string, width int) string {
	r := []rune(s)
	if width < 2 || len(r) < width {
		return s
	}
	return string(r[:width-1])
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
