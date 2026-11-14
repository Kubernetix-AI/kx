package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const secretMask = "••••••••••••••••" //nolint:gosec // G101: the mask shown after a paste, not a credential

func (p *Printer) reader() *bufio.Reader {
	if p.in.r == nil || p.in.src != p.In {
		p.in.src, p.in.r = p.In, bufio.NewReader(p.In)
	}
	return p.in.r
}

// ReadLine reads one line from stdin, trimmed (for --token-stdin). It
// returns ctx's error as soon as ctx ends.
func (p *Printer) ReadLine(ctx context.Context) (string, error) {
	return p.readCtx(ctx, p.readLine)
}

func (p *Printer) readLine() (string, error) {
	line, err := p.reader().ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ReadSecret prompts on stderr and reads a line from the terminal without
// echoing it, then shows a fixed-width mask so the person sees that
// something was pasted. When ctx ends (Ctrl-C) it returns at once and puts
// the terminal back the way it was.
func (p *Printer) ReadSecret(ctx context.Context, prompt string) (string, error) {
	if !p.InTTY || p.ReadPassword == nil {
		return "", ErrNoTTY
	}
	p.writeErr(prompt)
	s, err := p.readCtx(ctx, p.ReadPassword)
	if err != nil {
		if ctx.Err() == nil {
			p.writeErr("\n")
		}
		return "", err
	}
	p.writeErr(secretMask + "\n")
	return strings.TrimSpace(s), nil
}

// readCtx runs read, a blocking read from the terminal, and returns early
// with ctx's error when ctx ends. Go keeps a process alive on Ctrl-C while
// kx handles the signal, so without this a prompt would wait for Enter.
// The read is left behind (the process is about to exit), so the terminal
// is first put back the way it was: a no-echo read would otherwise leave
// echo off.
func (p *Printer) readCtx(ctx context.Context, read func() (string, error)) (string, error) {
	restore := func() {}
	if p.saveTerminal != nil {
		restore = p.saveTerminal()
	}
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := read()
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-ctx.Done():
		restore()
		p.writeErr("\n")
		return "", ctx.Err()
	}
}

// Confirm asks a yes/no question on stderr; the default is no.
func (p *Printer) Confirm(ctx context.Context, question string) (bool, error) {
	if !p.InTTY {
		return false, ErrNoTTY
	}
	p.writeErr(question + " [y/N] ")
	line, err := p.ReadLine(ctx)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// ErrNoChoice means a picker without a default ended before anything was
// chosen.
var ErrNoChoice = errors.New("nothing was chosen")

// Choose shows options and returns the chosen index; def is preselected,
// and a def of -1 preselects nothing, so Enter alone picks nothing and
// an option must be chosen by moving to it or typing its number. On a
// terminal it is an arrow-key picker (digits and j/k work too, and Ctrl-C
// cancels); if the terminal cannot enter raw mode or move the cursor it
// falls back to a numbered list, which returns ctx's error as soon as ctx
// ends.
func (p *Printer) Choose(ctx context.Context, question string, options []string, def int) (int, error) {
	if !p.InTTY || !p.TTY || len(options) == 0 {
		return -1, ErrNoTTY
	}
	if def < -1 || def >= len(options) {
		def = 0
	}
	if p.rawMode == nil || !p.Animate {
		return p.chooseByNumber(ctx, question, options, def)
	}
	restore, err := p.rawMode()
	if err != nil {
		return p.chooseByNumber(ctx, question, options, def)
	}
	defer restore()

	sel := def
	up := func() {
		if sel < 0 {
			sel = 0
		}
		sel = (sel - 1 + len(options)) % len(options)
	}
	down := func() { sel = (sel + 1) % len(options) }
	if def < 0 {
		question += " (use the arrow keys or a number, then Enter)"
	}
	draw := func(redraw bool) {
		var b strings.Builder
		if redraw {
			fmt.Fprintf(&b, "\x1b[%dA", len(options))
		}
		for i, o := range options {
			marker := "  "
			if i == sel {
				marker = p.paint(green, ">") + " "
				o = p.paint(bold, o)
			}
			b.WriteString("\r\x1b[2K" + marker + o + "\r\n")
		}
		p.writeErr(b.String())
	}
	p.writeErr(question + "\r\n")
	draw(false)

	r := p.reader()
	for {
		c, err := r.ReadByte()
		if err != nil {
			return -1, context.Canceled
		}
		switch c {
		case '\r', '\n':
			if sel >= 0 {
				return sel, nil
			}
		case 3, 4: // Ctrl-C, Ctrl-D
			return -1, context.Canceled
		case 'k':
			up()
		case 'j':
			down()
		case 0x1b:
			b1, err1 := r.ReadByte()
			b2, err2 := r.ReadByte()
			if err1 != nil || err2 != nil {
				return -1, context.Canceled
			}
			if b1 == '[' || b1 == 'O' {
				switch b2 {
				case 'A':
					up()
				case 'B':
					down()
				}
			}
		default:
			if c >= '1' && c <= '9' && int(c-'1') < len(options) {
				sel = int(c - '1')
			}
		}
		draw(true)
	}
}

// chooseByNumber is Choose as a numbered list. A number picks its option;
// an empty line takes the default, if there is one; anything else (a typo,
// a number out of range) asks again. Input that ends without an answer
// returns ErrNoChoice, with or without a default.
func (p *Printer) chooseByNumber(ctx context.Context, question string, options []string, def int) (int, error) {
	var b strings.Builder
	b.WriteString(question + "\n")
	for i, o := range options {
		fmt.Fprintf(&b, "  %d) %s\n", i+1, o)
	}
	again := fmt.Sprintf("Choose a number from 1 to %d: ", len(options))
	if def >= 0 {
		fmt.Fprintf(&b, "Choose [%d]: ", def+1)
		again = fmt.Sprintf("Choose a number from 1 to %d [%d]: ", len(options), def+1)
	} else {
		b.WriteString("Choose a number: ")
	}
	p.writeErr(b.String())
	for {
		line, err := p.ReadLine(ctx)
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
		n, convErr := strconv.Atoi(line)
		switch {
		case convErr == nil && n >= 1 && n <= len(options):
			return n - 1, nil
		case err != nil: // the input ended
			p.writeErr("\n")
			return -1, ErrNoChoice
		case line == "" && def >= 0:
			return def, nil
		}
		p.writeErr(again)
	}
}
