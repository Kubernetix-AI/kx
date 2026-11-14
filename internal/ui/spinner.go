package ui

import (
	"fmt"
	"sync"
	"time"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner shows that kx is still working. On a terminal that can redraw a
// line it animates on one line; elsewhere (CI logs, TERM=dumb, Windows
// consoles without virtual terminal output) it prints "<label>… <elapsed>"
// every HeartbeatEvery (10 s), so output never goes quiet for longer.
type Spinner struct {
	p     *Printer
	start time.Time
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once

	mu    sync.Mutex
	label string
}

// Spinner starts a spinner with label. Call Stop before printing the
// step's result.
func (p *Printer) Spinner(label string) *Spinner {
	s := &Spinner{p: p, label: label, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

// Resume starts a stopped spinner again with its label and its start
// time, so lines can be printed in between without the elapsed time
// starting over.
func (s *Spinner) Resume() *Spinner {
	s.mu.Lock()
	label := s.label
	s.mu.Unlock()
	n := &Spinner{p: s.p, label: label, start: s.start, stop: make(chan struct{}), done: make(chan struct{})}
	go n.run()
	return n
}

// Update changes the label.
func (s *Spinner) Update(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
}

// Println prints a progress line above the spinner, which carries on
// below it.
func (s *Spinner) Println(format string, args ...any) {
	line := fmt.Sprintf(format, cleanArgs(args)...) + "\n"
	if s.p.Animate {
		line = "\r\x1b[2K" + line
	}
	s.p.writeErr(line)
}

// Elapsed is the time since the spinner started.
func (s *Spinner) Elapsed() time.Duration { return time.Since(s.start) }

// Stop ends the spinner and clears its line on a terminal. It is safe to
// call more than once.
func (s *Spinner) Stop() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		if s.p.Animate {
			s.p.writeErr("\r\x1b[2K")
		}
	})
}

func (s *Spinner) run() {
	defer close(s.done)
	every := s.p.HeartbeatEvery
	if s.p.Animate {
		every = s.p.SpinEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		label := s.label
		s.mu.Unlock()
		elapsed := Duration(time.Since(s.start))
		if s.p.Animate {
			s.p.writeErr(fmt.Sprintf("\r\x1b[2K%s %s  %s", s.p.paint(green, frames[i%len(frames)]), label, s.p.Dim(elapsed)))
		} else {
			s.p.writeErr(fmt.Sprintf("  %s… %s\n", label, elapsed))
		}
	}
}
