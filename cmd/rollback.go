package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/ci"
	"github.com/kubernetix-ai/kx/internal/deploy"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/uuid"
)

func init() { register(newRollbackCmd) }

type rollbackOpts struct {
	app string
	to  string
	yes bool
}

func newRollbackCmd(a *App) *cobra.Command {
	var o rollbackOpts
	cmd := &cobra.Command{
		Use:     "rollback",
		Short:   "Put the previous version back",
		GroupID: groupApps,
		Long: `Redeploy the newest earlier version that went live with a different image, as a new
version. kx asks first; in scripts and CI, add --yes.`,
		Example: `  kx rollback                  asks before rolling back
  kx rollback --to v13 --yes   roll back to v13 without asking
  kx rollback --app shop-api`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRollback(cmd.Context(), a, cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.app, "app", "", "the `app` (default: the one this folder is linked to)")
	f.StringVar(&o.to, "to", "", "roll back to this `version`, like v13")
	f.BoolVarP(&o.yes, "yes", "y", false, "don't ask first")
	return cmd
}

func runRollback(ctx context.Context, a *App, cmd *cobra.Command, o rollbackOpts) error {
	to := 0
	if o.to != "" {
		m := versionArgRe.FindStringSubmatch(o.to)
		if m == nil {
			return &kxerr.Usage{What: "kx rollback --to takes a version like v13.", Fix: "See recent versions: kx status"}
		}
		to, _ = strconv.Atoi(m[1])
	}
	s, err := a.session(cmd, o.app)
	if err != nil {
		return err
	}
	if !o.yes && !s.P.InTTY {
		return &kxerr.Usage{What: "kx rollback needs --yes when it can't ask first.", Fix: "Run: kx rollback --yes"}
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	c, err := s.Client(ctx)
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	key, err := uuid.V4()
	if err != nil {
		return err
	}
	opts := deploy.RollbackOptions{App: app, Org: org, To: to, IdempotencyKey: key, Now: a.Now, Sleep: a.Sleep, StopCommands: s.CI.Provider == ci.GitHub}
	opts.Confirm = func(plan *deploy.RollbackPlan) (bool, error) {
		if o.yes {
			s.P.Info("Rolling %s back from v%d to v%d.", plan.App.Name, plan.From.Version, plan.To.Version)
			return true, nil
		}
		return s.P.Confirm(ctx, fmt.Sprintf("Roll %s back from v%d to v%d?", plan.App.Name, plan.From.Version, plan.To.Version))
	}
	res, err := deploy.Rollback(ctx, c, opts, *s.P)
	if errors.Is(err, deploy.ErrDeclined) {
		s.P.Info("Nothing changed.")
		return nil
	}
	if err != nil {
		return err
	}
	if a.jsonOut {
		return s.P.PrintJSON(deployJSON{res.App, res.AppID, res.Org, res.URL, res.DeploymentID, res.Version, res.Status, res.Image, false})
	}
	if res.URL != "" {
		s.P.Result(res.URL)
	}
	return nil
}
