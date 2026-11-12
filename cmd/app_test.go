package cmd

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// After the first Ctrl-C kx stops catching signals, so a second Ctrl-C
// ends it at once even if some code isn't watching the context.
func TestInterruptible_StopsCatchingAfterTheFirstSignal(t *testing.T) {
	stopped := make(chan struct{})
	var once sync.Once
	var signal context.CancelFunc
	var caught []os.Signal
	notify := func(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
		caught = sigs
		ctx, cancel := context.WithCancel(parent)
		signal = cancel
		return ctx, func() { once.Do(func() { close(stopped) }); cancel() }
	}
	ctx, stop := interruptible(context.Background(), notify)
	defer stop()
	if len(caught) == 0 || caught[0] != os.Interrupt {
		t.Fatalf("catches %v, want Ctrl-C first", caught)
	}
	select {
	case <-stopped:
		t.Fatal("stopped catching signals before any arrived")
	case <-time.After(20 * time.Millisecond):
	}
	signal() // the first Ctrl-C
	<-ctx.Done()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("still catching signals after the first one")
	}
}
