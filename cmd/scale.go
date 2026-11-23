package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func init() { register(newScaleCmd) }

func newScaleCmd(a *App) *cobra.Command {
	var f configureFlags
	cmd := &cobra.Command{
		Use:     "scale <replicas>",
		Short:   "Set how many replicas the app runs, then deploy it",
		GroupID: groupMore,
		Long: `Set how many replicas (copies) of the app run, then deploy the app as configured and wait
until it's live, like kx deploy. Your plan sets the most an app can run.`,
		Example: `  kx scale 3
  kx scale 1 --app shop-api
  kx scale 2 --no-deploy        save it; the next deploy applies it`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return asChange(runScale(cmd.Context(), a, cmd, f, args))
		},
	}
	addConfigureFlags(cmd, &f)
	return cmd
}

func runScale(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, args []string) error {
	bad := &kxerr.Usage{What: "kx scale takes how many replicas to run, a whole number from 1 up.", Fix: "Run: kx scale 2"}
	if len(args) != 1 {
		return bad
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		return bad
	}
	plural := "replicas"
	if n == 1 {
		plural = "replica"
	}
	return runConfigure(ctx, a, cmd, f, appChange{
		command: "kx scale",
		prepare: func(ctx context.Context, c *api.Client, app *api.App) (string, error) {
			if app.Autoscales() {
				what := fmt.Sprintf("%s autoscales between %d and %d replicas, so a replica count has no effect while autoscaling is on.",
					app.Name, app.Autoscaling.Min(), app.Autoscaling.MaxReplicas)
				if !f.noDeploy {
					// Deploying would use up a deploy and change nothing.
					return "", &kxerr.Usage{What: what, Fix: fmt.Sprintf("To save the count for when autoscaling is off: kx scale %d --no-deploy", n)}
				}
				a.printer().Warn("%s The count applies once autoscaling is off.", what)
				return "", nil
			}
			if app.Replicas != n {
				return "", nil
			}
			// The app is set to n already. What's live may still run another
			// count (n saved without a deploy): then deploying changes it.
			var live *api.Deployment
			if deps, err := c.ListDeployments(ctx, app.ID, 20); err == nil {
				live = liveDeployment(deps)
			}
			if live != nil && live.Replicas == n {
				return fmt.Sprintf("Nothing to change: %s already runs %d %s.", app.Name, n, plural), nil
			}
			if !f.noDeploy && live != nil && live.Replicas > 0 {
				return "", nil
			}
			return fmt.Sprintf("Nothing to change: %s is already set to %d %s.", app.Name, n, plural), nil
		},
		step: func(app *api.App) (string, string) { return "Scaled", fmt.Sprintf("%s to %d %s", app.Name, n, plural) },
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.SetReplicas(ctx, app.ID, n)
			return err
		},
		undone: func(*api.App) func(*api.App) (bool, bool) {
			return func(fresh *api.App) (bool, bool) { return fresh.Replicas != n, true }
		},
		out: func(o *configureJSON) { o.Replicas = n },
	})
}
