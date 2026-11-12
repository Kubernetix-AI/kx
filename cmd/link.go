package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() { register(newLinkCmd) }

func newLinkCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "link [app]",
		Aliases: []string{"init"},
		Short:   "Choose the app this folder deploys to",
		GroupID: groupStart,
		Long: `Save the app this folder deploys to in .kx.json, so later commands need no --app.
.kx.json holds no secret, so you can commit it for your team and CI.`,
		Example: `  kx link              pick from your apps (the folder's name comes first)
  kx link shop-api     link to shop-api`,
		Args: usageArgs(0, 1, "an app name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runLink(cmd.Context(), a, cmd, name)
		},
	}
}

// linkChoice is the outcome of choosing an app: an existing app, or the
// name of a new one that kx deploy will create.
type linkChoice struct {
	app    *api.App
	newApp string
}

func runLink(ctx context.Context, a *App, cmd *cobra.Command, name string) error {
	if name != "" && !config.ValidAppName(name) {
		return &kxerr.Usage{What: fmt.Sprintf("%q isn't an app name; app names look like shop-api.", name), Fix: "See your apps: kx apps list"}
	}
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	choice, err := chooseApp(ctx, s, name)
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	proj := config.Project{App: choice.newApp, Org: org}
	if choice.app != nil {
		proj.App, proj.AppID = choice.app.Name, choice.app.ID
	}
	path, err := config.SaveProject(s.Cfg.Dir, proj)
	if err != nil {
		return fmt.Errorf("saving .kx.json: %w", err)
	}
	if a.jsonOut {
		return s.P.PrintJSON(struct {
			App   string `json:"app"`
			AppID string `json:"app_id,omitempty"`
			Org   string `json:"org"`
			Path  string `json:"path"`
		}{proj.App, proj.AppID, proj.Org, path})
	}
	if choice.newApp != "" {
		s.P.Info("%s doesn't exist yet; kx deploy will create it.", choice.newApp)
	}
	s.P.Info("Linked this folder to %s in %s (saved in .kx.json).", proj.App, org)
	s.P.Info("Next: kx deploy")
	return nil
}

// chooseApp picks the app for this folder. An app-scoped token decides by
// itself. Otherwise name (if given) must exist, or, for a token that can
// deploy every app, becomes a new app. With no name, kx asks, putting the
// folder's name first (as "(new app)" when it does not exist yet).
func chooseApp(ctx context.Context, s *Session, name string) (linkChoice, error) {
	c, err := s.Client()
	if err != nil {
		return linkChoice{}, err
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return linkChoice{}, err
	}
	if t := who.Token; t != nil && t.AppScoped() {
		if name != "" && name != t.AppName {
			return linkChoice{}, &kxerr.TokenAppMismatch{TokenApp: t.AppName, App: name}
		}
		app, err := c.GetApp(ctx, t.AppID)
		return linkChoice{app: app}, err
	}
	canCreate := who.Token == nil || who.Token.Can(api.PermDeploy)
	if _, err := s.Org(ctx); err != nil {
		return linkChoice{}, err
	}

	if name != "" {
		app, err := s.FindApp(ctx, name, "")
		var apiErr *api.Error
		if err != nil && canCreate && errors.As(err, &apiErr) && apiErr.Code == "app_not_found" {
			return linkChoice{newApp: name}, nil
		}
		return linkChoice{app: app}, err
	}

	apps, err := c.ListApps(ctx)
	if err != nil {
		return linkChoice{}, err
	}
	folder := filepath.Base(s.Cfg.Dir)
	var labels []string
	var choices []linkChoice
	def := 0
	if canCreate && config.ValidAppName(folder) && !hasApp(apps, folder) {
		labels = append(labels, folder+" (new app)")
		choices = append(choices, linkChoice{newApp: folder})
	}
	for i := range apps {
		if apps[i].Name == folder {
			def = len(choices)
		}
		labels = append(labels, apps[i].Name)
		choices = append(choices, linkChoice{app: &apps[i]})
	}
	if len(choices) == 0 {
		return linkChoice{}, &kxerr.Usage{
			What: "There are no apps to link to yet.",
			Fix:  "Create an app in the dashboard, or log in with a token that can deploy: kx login",
		}
	}
	i, err := s.P.Choose(ctx, "Which app should this folder deploy to?", labels, def)
	if errors.Is(err, ui.ErrNoTTY) {
		return linkChoice{}, &kxerr.Usage{What: "kx link needs an app name when it can't ask.", Fix: "Run: kx link <app>"}
	}
	if err != nil {
		return linkChoice{}, err
	}
	return choices[i], nil
}

func hasApp(apps []api.App, name string) bool {
	for _, a := range apps {
		if a.Name == name {
			return true
		}
	}
	return false
}
