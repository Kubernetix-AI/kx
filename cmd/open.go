package cmd

import (
	"context"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func init() { register(newOpenCmd) }

func newOpenCmd(a *App) *cobra.Command {
	var app string
	var dashboard bool
	cmd := &cobra.Command{
		Use:     "open",
		Short:   "Open your app in the browser",
		GroupID: groupApps,
		Example: `  kx open               open the live URL
  kx open --dashboard   open the app's page in the dashboard`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOpen(cmd.Context(), a, cmd, app, dashboard)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "the `app` (default: the one this folder is linked to)")
	cmd.Flags().BoolVar(&dashboard, "dashboard", false, "open the app's dashboard page instead")
	return cmd
}

func runOpen(ctx context.Context, a *App, cmd *cobra.Command, appFlag string, dashboard bool) error {
	s, err := a.session(cmd, appFlag)
	if err != nil {
		return err
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	target := app.URL
	if dashboard {
		target = s.DashboardURL("/apps/" + url.PathEscape(app.ID))
	} else if target == "" {
		return &kxerr.NotLive{App: app.Name}
	}
	if a.jsonOut {
		return s.P.PrintJSON(struct {
			URL string `json:"url"`
		}{target})
	}
	if a.CanOpenBrowser() {
		_ = a.OpenBrowser(target)
	}
	s.P.Result(target)
	return nil
}
