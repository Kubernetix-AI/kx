package cmd

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/safetext"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() { register(newLogsCmd) }

const (
	logsTail      = 100
	logsFollowMax = 1000
	followEvery   = 2 * time.Second
)

type logsOpts struct {
	app        string
	follow     bool
	since      string
	deployment string
}

func newLogsCmd(a *App) *cobra.Command {
	var o logsOpts
	cmd := &cobra.Command{
		Use:     "logs",
		Short:   "Show your app's output",
		GroupID: groupApps,
		Long:    "Show the last 100 lines your app printed in the past hour, or a deployment's logs.",
		Example: `  kx logs                      the last 100 lines
  kx logs --follow             keep watching (Ctrl-C to stop)
  kx logs --since 2d           go back two days
  kx logs --deployment v15     why v15 didn't go live`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd.Context(), a, cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.app, "app", "", "the `app` (default: the one this folder is linked to)")
	f.BoolVarP(&o.follow, "follow", "f", false, "keep printing new lines as they arrive")
	f.StringVar(&o.since, "since", "1h", "how far back to start, as a `time` like 30m, 2h or 2d")
	f.StringVar(&o.deployment, "deployment", "", "show the logs of this deployment `version` instead, like v15")
	return cmd
}

func runLogs(ctx context.Context, a *App, cmd *cobra.Command, o logsOpts) error {
	if o.deployment != "" && o.follow {
		return &kxerr.Usage{What: "kx logs --follow watches your app's output, not a deployment's logs.", Fix: "Run: kx logs --follow"}
	}
	since, ok := parseSince(o.since)
	if !ok {
		return &kxerr.Usage{What: "kx logs --since takes a length of time like 30m, 2h or 2d.", Fix: "Run: kx logs --since 2h"}
	}
	s, err := a.session(cmd, o.app)
	if err != nil {
		return err
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	c, _ := s.Client(ctx)
	if o.deployment != "" {
		return deploymentLogs(ctx, s, c, app, o.deployment)
	}

	start := a.Now().Add(-since)
	lines, err := c.RuntimeLogs(ctx, app.ID, api.LogQuery{Limit: logsTail, Start: start})
	if err != nil {
		return err
	}
	printLogLines(s, lines)
	if !o.follow {
		return nil
	}

	// Follow: ask for everything from the newest line seen, forwards, and
	// skip what was already printed at that exact timestamp.
	last := start.UnixNano()
	seen := map[string]int{}
	if n := len(lines); n > 0 {
		last = lines[n-1].NS
		for _, l := range lines {
			if l.NS == last {
				seen[l.Line]++
			}
		}
	}
	for {
		if err := a.Sleep(ctx, followEvery); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		batch, err := c.RuntimeLogs(ctx, app.ID, api.LogQuery{Limit: logsFollowMax, Start: time.Unix(0, last), Forward: true})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		skip := make(map[string]int, len(seen))
		for k, v := range seen {
			skip[k] = v
		}
		var fresh []api.LogLine
		for _, l := range batch {
			if l.NS < last {
				continue
			}
			if l.NS == last && skip[l.Line] > 0 {
				skip[l.Line]--
				continue
			}
			fresh = append(fresh, l)
			if l.NS > last {
				last, seen = l.NS, map[string]int{}
			}
			seen[l.Line]++
		}
		printLogLines(s, fresh)
	}
}

// printLogLines prints a batch of the app's output on stdout.
func printLogLines(s *Session, lines []api.LogLine) {
	if s.App.jsonOut {
		for _, l := range lines {
			_ = s.P.PrintJSONLine(struct {
				Time string `json:"time"`
				Line string `json:"line"`
			}{l.Time.UTC().Format(time.RFC3339Nano), cleanLogText(l.Line)})
		}
		return
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = safetext.Neutralize(ui.CleanText(l.Line))
	}
	defer stopCommands(s, texts)()
	for i, l := range lines {
		s.P.LogLine(l.Time.Local().Format("2006-01-02 15:04:05"), texts[i])
	}
}

// stopCommands keeps a CI runner from acting on a block of the app's
// output, whose lines are already neutralised: in GitHub Actions it starts
// a ::stop-commands:: block with a fresh random token, as kx deploy does,
// and returns the function that ends it. Both lines go to stdout with the
// app's output, so the runner reads them in order.
func stopCommands(s *Session, lines []string) (end func()) {
	if len(lines) == 0 || s.CI.Provider != ci.GitHub {
		return func() {}
	}
	tok := safetext.StopToken(lines)
	s.P.Result("::stop-commands::" + tok)
	return func() { s.P.Result("::" + tok + "::") }
}

var versionArgRe = regexp.MustCompile(`^[vV]?([0-9]{1,9})$`)
var deploymentIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

func deploymentLogs(ctx context.Context, s *Session, c *api.Client, app *api.App, which string) error {
	var dep *api.Deployment
	if m := versionArgRe.FindStringSubmatch(which); m != nil {
		v, _ := strconv.Atoi(m[1])
		deps, err := c.ListDeployments(ctx, app.ID, 100)
		if err != nil {
			return err
		}
		for i := range deps {
			if deps[i].Version == v {
				dep = &deps[i]
				break
			}
		}
		if dep == nil {
			return &kxerr.NoVersion{App: app.Name, Version: fmt.Sprintf("v%d", v)}
		}
	} else if deploymentIDRe.MatchString(which) {
		d, err := c.GetDeployment(ctx, which)
		if err != nil {
			return err
		}
		dep = d
	} else {
		return &kxerr.Usage{What: "kx logs --deployment takes a version like v15.", Fix: "See recent versions: kx status"}
	}

	logs, err := c.DeploymentLogs(ctx, dep.ID)
	if err != nil {
		return err
	}
	text := strings.TrimRight(strings.Join(nonEmpty(logs.BuildLogs, logs.DeploymentLogs), "\n"), "\n")
	reason := cleanLogText(dep.CustomerError)

	if s.App.jsonOut {
		return s.P.PrintJSON(struct {
			App            string `json:"app"`
			Version        int    `json:"version"`
			Status         string `json:"status"`
			Reason         string `json:"reason,omitempty"`
			BuildLogs      string `json:"build_logs"`
			DeploymentLogs string `json:"deployment_logs"`
		}{app.Name, dep.Version, dep.Status, reason, logs.BuildLogs, logs.DeploymentLogs})
	}
	label := fmt.Sprintf("%s v%d", app.Name, dep.Version)
	switch {
	case dep.Status == api.StatusFailed && reason != "":
		s.P.Info("%s failed: %s.", label, strings.TrimRight(safetext.Neutralize(reason), ". "))
	case dep.Status == api.StatusFailed:
		s.P.Info("%s failed.", label)
	case dep.Status == api.StatusReady:
		s.P.Info("%s went live.", label)
	case dep.Status == api.StatusRolledBack:
		s.P.Info("%s was rolled back.", label)
	default:
		s.P.Info("%s is %s.", label, dep.Phase())
	}
	if text == "" {
		s.P.Info("There are no logs for %s yet.", label)
		return nil
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, safetext.Neutralize(cleanLogText(line)))
	}
	defer stopCommands(s, lines)()
	for _, line := range lines {
		s.P.Result(line)
	}
	return nil
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// cleanLogText drops control characters (keeping tabs), so app output
// cannot move the cursor or retitle the terminal.
func cleanLogText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// parseSince accepts Go durations (90s, 30m, 2h) and whole days (2d), up
// to 30 days.
func parseSince(s string) (time.Duration, bool) {
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, false
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, false
		}
	}
	return d, d > 0 && d <= 30*24*time.Hour
}
