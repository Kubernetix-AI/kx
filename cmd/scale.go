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
			return runScale(cmd.Context(), a, cmd, f, args)
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
		check: func(app *api.App) {
			if app.Autoscales() {
				a.printer().Warn("%s autoscales between %d and %d replicas, so autoscaling decides how many run; the count you set applies once autoscaling is off.",
					app.Name, app.Autoscaling.Min(), app.Autoscaling.MaxReplicas)
			}
		},
		step: func(app *api.App) (string, string) { return "Scaled", fmt.Sprintf("%s to %d %s", app.Name, n, plural) },
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.SetReplicas(ctx, app.ID, n)
			return err
		},
		out: func(o *configureJSON) { o.Replicas = n },
	})
}
