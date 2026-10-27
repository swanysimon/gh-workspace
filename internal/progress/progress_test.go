package progress

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// screen interprets exactly the escape sequences Tracker writes, so tests
// assert on what a terminal would show rather than on the byte stream.
type screen struct {
	rows []string
	row  int
	mid  bool // the cursor is past column 0 of row
}

var escape = regexp.MustCompile(`^\x1b\[(\d*)([FKJ])`)

func (s *screen) Write(p []byte) (int, error) {
	in := string(p)
	for len(in) > 0 {
		if m := escape.FindStringSubmatch(in); m != nil {
			n, _ := strconv.Atoi(m[1])
			switch m[2] {
			case "F":
				s.row, s.mid = s.row-n, false
			case "J":
				keep := s.row
				if s.mid {
					keep++
				}
				s.rows = s.rows[:min(keep, len(s.rows))]
			}
			in = in[len(m[0]):]
			continue
		}
		if in[0] == '\n' {
			s.row, s.mid = s.row+1, false
			in = in[1:]
			continue
		}
		end := strings.IndexAny(in, "\x1b\n")
		if end < 0 {
			end = len(in)
		}
		for len(s.rows) <= s.row {
			s.rows = append(s.rows, "")
		}
		if s.mid {
			s.rows[s.row] += in[:end]
		} else {
			s.rows[s.row] = in[:end]
		}
		s.mid = true
		in = in[end:]
	}
	return len(p), nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time            { return c.t }
func (c *clock) advance(d time.Duration)   { c.t = c.t.Add(d) }
func fixedSize(w, h int) func() (int, int) { return func() (int, int) { return w, h } }

func liveTracker(total int, persist bool) (*Tracker, *screen, *clock) {
	scr := &screen{}
	c := &clock{t: time.Unix(0, 0)}
	return newTracker(scr, total, persist, true, c.now, fixedSize(80, 24)), scr, c
}

func assertRows(t *testing.T, scr *screen, want ...string) {
	t.Helper()
	if strings.Join(scr.rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("screen:\n%s\nwant:\n%s", strings.Join(scr.rows, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlainWritesOneLinePerStart(t *testing.T) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(0, 0)}
	tr := newTracker(&buf, 2, false, false, c.now, fixedSize(80, 24))
	a := tr.Start("o/a", "fetching")
	tr.Start("o/b", "cloning")
	a.Done(nil)
	tr.Stop()
	want := "[1/2] o/a: fetching\n[2/2] o/b: cloning\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestPlainPersistAddsFinishLines(t *testing.T) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(0, 0)}
	tr := newTracker(&buf, 2, true, false, c.now, fixedSize(80, 24))
	a := tr.Start("o/a", "fetching")
	b := tr.Start("o/b", "cloning")
	c.advance(1500 * time.Millisecond)
	b.Done(errors.New("boom"))
	a.Done(nil)
	want := "[1/2] o/a: fetching\n[2/2] o/b: cloning\n" +
		"[1/2] o/b: cloning failed after 1.5s\n[2/2] o/a: fetching done in 1.5s\n"
	if buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestLiveBlockRedrawsInPlace(t *testing.T) {
	tr, scr, c := liveTracker(3, false)
	a := tr.Start("o/a", "fetching")
	tr.Start("o/long-name", "cloning")
	c.advance(2 * time.Second)
	tr.redraw()
	assertRows(t, scr,
		"⠋ o/a          fetching  2.0s",
		"⠋ o/long-name  cloning   2.0s",
		"[0/3] 2 running  2.0s",
	)

	a.Done(errors.New("boom"))
	tr.frame++
	tr.redraw()
	assertRows(t, scr,
		"⠙ o/long-name  cloning  2.0s",
		"[1/3] 1 running, 1 failed  2.0s",
	)
}

func TestLiveStopErasesBlock(t *testing.T) {
	tr, scr, _ := liveTracker(2, false)
	tr.Start("o/a", "fetching").Done(nil)
	tr.Start("o/b", "fetching")
	tr.redraw()
	tr.clear()
	assertRows(t, scr)
}

func TestLivePersistScrollsFinishedAboveBlock(t *testing.T) {
	tr, scr, c := liveTracker(2, true)
	a := tr.Start("o/a", "fetching")
	tr.Start("o/b", "cloning")
	tr.redraw()
	c.advance(time.Second)
	a.Done(nil)
	assertRows(t, scr,
		"✓ o/a fetching 1.0s",
		"⠋ o/b  cloning  1.0s",
		"[1/2] 1 running  1.0s",
	)
	tr.Start("o/c", "fetching").Done(errors.New("boom"))
	tr.clear()
	assertRows(t, scr, "✓ o/a fetching 1.0s", "✗ o/c fetching failed after 0.0s")
}

func TestPauseHoldsPersistedLinesUntilResume(t *testing.T) {
	tr, scr, _ := liveTracker(2, true)
	a := tr.Start("o/a", "archiving")
	tr.Start("o/b", "fetching")
	tr.redraw()
	resume := tr.Pause()
	assertRows(t, scr)

	a.Done(nil)
	assertRows(t, scr)

	resume()
	resume()
	assertRows(t, scr,
		"✓ o/a archiving 0.0s",
		"⠋ o/b  fetching  0.0s",
		"[1/2] 1 running  0.0s",
	)
}

func TestSingleItemHasNoCount(t *testing.T) {
	tr, scr, _ := liveTracker(1, false)
	tr.Start("o/a", "cloning")
	tr.redraw()
	assertRows(t, scr, "⠋ o/a  cloning  0.0s")
}

func TestBlockFitsTerminal(t *testing.T) {
	scr := &screen{}
	c := &clock{t: time.Unix(0, 0)}
	tr := newTracker(scr, 10, false, true, c.now, fixedSize(20, 5))
	for _, name := range []string{"o/a", "o/b", "o/c", "o/d", "o/e"} {
		tr.Start(name, "fetching")
	}
	tr.redraw()
	assertRows(t, scr,
		"⠋ o/a  fetching  0.",
		"⠋ o/b  fetching  0.",
		"  +3 more",
		"[0/10] 5 running  0",
	)
}

func TestFormatElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{
		250 * time.Millisecond:  "0.2s",
		59 * time.Second:        "59.0s",
		61 * time.Second:        "1m01s",
		125*time.Minute + 500e6: "125m01s",
	} {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLiveConcurrentUseThenStop(t *testing.T) {
	scr := &screen{}
	tr := newTracker(scr, 50, true, true, time.Now, fixedSize(80, 24))
	go tr.loop()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() {
			it := tr.Start("o/r"+strconv.Itoa(i), "fetching")
			time.Sleep(time.Millisecond)
			it.Done(nil)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	tr.Stop()
	if len(scr.rows) != 50 {
		t.Fatalf("want 50 persisted rows and no block, got %d:\n%s", len(scr.rows), strings.Join(scr.rows, "\n"))
	}
}
