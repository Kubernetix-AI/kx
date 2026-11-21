// Package cmd holds kx's commands.
//
// Each command lives in its own file and registers itself from init with
// register(newXCmd), so adding one (deploy, rollback) is a one-file change.
// Commands reach the outside world only through *App, which tests replace.
package cmd

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/browser"
	"github.com/kubernetix-ai/kx/internal/deploy"
	"github.com/kubernetix-ai/kx/internal/docker"
	imageregistry "github.com/kubernetix-ai/kx/internal/registry"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// App is everything a command needs from outside: streams, environment,
// clock, working directory, browser and HTTP. Zero fields get real
// defaults in Run.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Now    func() time.Time
	Dir    string // working directory; "" means os.Getwd

	OpenBrowser    func(url string) error
	CanOpenBrowser func() bool
	HTTPClient     *http.Client // nil keeps the api package's default
	Sleep          func(ctx context.Context, d time.Duration) error
	// Docker builds and pushes images; nil runs the docker command.
	Docker deploy.Builder
	// Registry looks up image tags; nil asks the registry over HTTPS.
	Registry deploy.Registry

	// PreparePrinter, if set, adjusts each new Printer (tests use it to
	// pretend there is a terminal).
	PreparePrinter func(*ui.Printer)

	// Set per run from the global flags.
	jsonOut bool
	orgFlag string
	apiURL  string

	// autoLoggedIn is set once a command has logged in by itself in this
	// run, which it does at most once.
	autoLoggedIn bool
}

// Main runs kx with the process's arguments and returns the exit code.
func Main() int {
	ctx, stop := interruptible(context.Background(), signal.NotifyContext)
	defer stop()
	return (&App{}).Run(ctx, os.Args[1:])
}

// interruptible returns a context that ends at the first Ctrl-C (or
// SIGTERM), so kx can stop cleanly. After that first signal kx stops
// catching them, so a second Ctrl-C ends kx at once even if some code
// isn't watching the context. notify is signal.NotifyContext.
func interruptible(parent context.Context, notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)) (context.Context, context.CancelFunc) {
	ctx, stop := notify(parent, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// Run executes one kx command line and returns its exit code. Errors are
// printed as two lines (or JSON with --json) through api.Explain.
func (a *App) Run(ctx context.Context, args []string) int {
	a.defaults()
	root := newRoot(a)
	if args == nil {
		args = []string{} // cobra reads os.Args when given nil
	}
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return api.ExitOK
	}
	if !a.jsonOut {
		a.jsonOut = hasJSONFlag(args)
	}
	prob := api.Explain(err)
	a.printer().Fail(prob.Code, prob.What, prob.Fix)
	return prob.Exit
}

func (a *App) defaults() {
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.OpenBrowser == nil {
		a.OpenBrowser = func(u string) error { return browser.Open(a.Getenv, u) }
	}
	if a.CanOpenBrowser == nil {
		a.CanOpenBrowser = func() bool { return browser.CanOpen(a.Getenv) }
	}
	if a.Sleep == nil {
		a.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
}

// docker returns the Docker the commands use.
func (a *App) docker() deploy.Builder {
	if a.Docker != nil {
		return a.Docker
	}
	return docker.Engine{}
}

// registry returns what the commands look image tags up with.
func (a *App) registry() deploy.Registry {
	if a.Registry != nil {
		return a.Registry
	}
	return imageregistry.Client{}
}

// printer returns a Printer for the current run (honouring --json).
func (a *App) printer() *ui.Printer {
	p := ui.New(a.Stdin, a.Stdout, a.Stderr, a.Getenv, a.jsonOut)
	if a.PreparePrinter != nil {
		a.PreparePrinter(p)
	}
	return p
}

func hasJSONFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--json" || arg == "--json=true" {
			return true
		}
	}
	return false
}
