package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
)

func init() { register(newStatusCmd) }

func newStatusCmd(a *App) *cobra.Command {
	var app string
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "See what's live: version, URL, replicas, deploys left today",
		GroupID: groupStart,
		Example: `  kx status
  kx status --app shop-api
  kx status --json`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context(), a, cmd, app)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "the `app` (default: the one this folder is linked to)")
	return cmd
}

type statusOut struct {
	App          string       `json:"app"`
	AppID        string       `json:"app_id"`
	LiveVersion  int          `json:"live_version"`
	URL          string       `json:"url"`
	Latest       *latestOut   `json:"latest,omitempty"`
	Replicas     *replicasOut `json:"replicas,omitempty"`
	DeploysToday *deploysOut  `json:"deploys_today,omitempty"`
}

type latestOut struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
}

type replicasOut struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
	// Starting means none are counted yet, but the newest version went
	// live moments ago: counts can take a minute to appear.
	Starting bool `json:"starting,omitempty"`
	// Desired is how many replicas the app is set to run; left out when
	// autoscaling decides.
	Desired int `json:"desired,omitempty"`
	// Stopping counts the replicas beyond Desired right after the newest
	// version went live: the version before it (StoppingVersion, when
	// known) shutting down.
	Stopping        int `json:"stopping,omitempty"`
	StoppingVersion int `json:"stopping_version,omitempty"`
	// StartingVersion is a newer version on its way while the live one
	// serves.
	StartingVersion int `json:"starting_version,omitempty"`
}

// countsLag is how long after a version goes live kx takes "no replicas"
// to mean the counts haven't caught up, not that nothing runs.
const countsLag = 3 * time.Minute

type deploysOut struct {
	Used  int    `json:"used"`
	Limit int    `json:"limit"`
	Plan  string `json:"plan,omitempty"`
}

func runStatus(ctx context.Context, a *App, cmd *cobra.Command, appFlag string) error {
	s, err := a.session(cmd, appFlag)
	if err != nil {
		return err
	}
	app, err := s.ResolveAppOrOnly(ctx, "status")
	if err != nil {
		return err
	}
	c, _ := s.Client(ctx)
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	deps, err := c.ListDeployments(ctx, app.ID, 20)
	if err != nil {
		return err
	}
	out := statusOut{App: app.Name, AppID: app.ID, URL: app.URL}
	live := liveDeployment(deps)
	if live != nil {
		out.LiveVersion = live.Version
	}
	if len(deps) > 0 {
		out.Latest = &latestOut{Version: deps[0].Version, Status: deps[0].Status}
	}
	// Replicas and usage are nice to have: a failure leaves them out.
	if reps, err := c.Replicas(ctx, app.ID); err == nil {
		r := &replicasOut{Total: len(reps)}
		for _, rep := range reps {
			if rep.Ready {
				r.Ready++
			}
		}
		r.Starting = r.Total == 0 && len(deps) > 0 && justWentLive(deps[0], a.Now())
		rollout(r, app, deps, live)
		out.Replicas = r
	}
	if u, err := c.Usage(ctx, org); err == nil {
		if used, limit, ok := u.DeploysToday(); ok {
			out.DeploysToday = &deploysOut{Used: used, Limit: limit, Plan: u.Plan}
		}
	}

	if a.jsonOut {
		return s.P.PrintJSON(out)
	}
	rows := [][]string{{"App", app.Name}}
	if live != nil {
		rows = append(rows, []string{"Live", fmt.Sprintf("v%d", live.Version)})
	} else {
		rows = append(rows, []string{"Live", "nothing yet"})
	}
	if out.Latest != nil && (live == nil || deps[0].ID != live.ID) {
		rows = append(rows, []string{"Newest", fmt.Sprintf("v%d %s", out.Latest.Version, (api.Deployment{Status: out.Latest.Status}).Phase())})
	}
	rows = append(rows, []string{"URL", dashIfEmpty(app.URL)})
	switch {
	case out.Replicas == nil:
		rows = append(rows, []string{"Replicas", "unknown"})
	case out.Replicas.Starting:
		rows = append(rows, []string{"Replicas", "starting (counts can take a minute to appear)"})
	case out.Replicas.Total == 0:
		rows = append(rows, []string{"Replicas", "none running"})
	case out.Replicas.Stopping > 0:
		r := out.Replicas
		line := fmt.Sprintf("v%d ready (%d of %d); ", live.Version, min(r.Ready, r.Desired), r.Desired)
		if r.StoppingVersion > 0 {
			line += fmt.Sprintf("v%d stopping", r.StoppingVersion)
		} else {
			line += "the version before it stopping"
		}
		rows = append(rows, []string{"Replicas", line})
	case out.Replicas.StartingVersion > 0:
		r := out.Replicas
		rows = append(rows, []string{"Replicas", fmt.Sprintf("v%d starting; v%d serving (%d of %d ready)", r.StartingVersion, live.Version, r.Ready, r.Total)})
	default:
		rows = append(rows, []string{"Replicas", fmt.Sprintf("%d of %d ready", out.Replicas.Ready, out.Replicas.Total)})
	}
	if d := out.DeploysToday; d != nil && d.Limit > 0 {
		left := d.Limit - d.Used
		if left < 0 {
			left = 0
		}
		line := fmt.Sprintf("%d of %d left today", left, d.Limit)
		if d.Plan != "" {
			line += " on the " + planName(d.Plan) + " plan"
		}
		rows = append(rows, []string{"Deploys", line})
	}
	s.P.Table(nil, rows)
	return nil
}

// justWentLive reports whether d went live less than countsLag before now
// (by completed_at, else updated_at).
func justWentLive(d api.Deployment, now time.Time) bool {
	at := d.UpdatedAt
	if d.CompletedAt != nil {
		at = *d.CompletedAt
	}
	return d.Status == api.StatusReady && !at.IsZero() && now.Sub(at) < countsLag
}

// rollout explains replica counts during a rollout. Kubernetix lists every
// replica, the old version's too, so right after a version goes live the
// count runs over the app's set number while the old ones shut down (which
// "1 of 2 ready" would make look like trouble), and while a new version
// starts the live one keeps serving. Apps that autoscale have no set number
// to compare with.
func rollout(r *replicasOut, app *api.App, deps []api.Deployment, live *api.Deployment) {
	if app.Autoscales() || app.Replicas < 1 {
		return
	}
	r.Desired = app.Replicas
	if live == nil || len(deps) == 0 || r.Total <= r.Desired {
		return
	}
	switch newest := deps[0]; {
	case newest.ID == live.ID:
		r.Stopping = r.Total - r.Desired
		for _, d := range deps[1:] {
			if d.Status == api.StatusReady {
				r.StoppingVersion = d.Version
				break
			}
		}
	case !newest.Done():
		r.StartingVersion = newest.Version
	}
}

// liveDeployment is the newest deployment that went live. Deployments are
// newest first.
func liveDeployment(deps []api.Deployment) *api.Deployment {
	for i := range deps {
		if deps[i].Status == api.StatusReady {
			return &deps[i]
		}
	}
	return nil
}

func planName(plan string) string {
	if plan == "" {
		return ""
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}
