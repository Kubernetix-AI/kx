package cmd

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() { register(newAppsCmd) }

func newAppsCmd(a *App) *cobra.Command {
	list := &cobra.Command{
		Use:     "list",
		Short:   "List your apps",
		Example: "  kx apps list\n  kx apps list --json",
		Args:    usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppsList(cmd.Context(), a, cmd)
		},
	}
	get := &cobra.Command{
		Use:     "get <app>",
		Short:   "Show one app",
		Example: "  kx apps get shop-api\n  kx apps get shop-api --json",
		Args:    usageArgs(1, 1, "an app name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppsGet(cmd.Context(), a, cmd, args[0])
		},
	}
	apps := &cobra.Command{
		Use:     "apps",
		Short:   "List and inspect apps",
		GroupID: groupMore,
		Example: "  kx apps                list your apps\n  kx apps get shop-api   show one app",
		Args:    usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppsList(cmd.Context(), a, cmd)
		},
	}
	apps.AddCommand(list, get)
	return apps
}

type appOut struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Version  int    `json:"version"`
	URL      string `json:"url"`
	Port     int    `json:"port"`
	Replicas int    `json:"replicas"`
	Image    string `json:"image,omitempty"`
	Created  string `json:"created_at,omitempty"`
}

// toAppOut shows app with version, its live version (0 for none).
func toAppOut(app api.App, version int) appOut {
	out := appOut{Name: app.Name, ID: app.ID, Status: app.Status, Version: version, URL: app.URL, Port: app.Port, Replicas: app.Replicas, Image: app.ImageURL}
	if !app.CreatedAt.IsZero() {
		out.Created = app.CreatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func runAppsList(ctx context.Context, a *App, cmd *cobra.Command) error {
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	c, err := s.Client(ctx)
	if err != nil {
		return err
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return err
	}
	var apps []api.App
	if t := who.Token; t != nil && t.AppScoped() {
		app, err := c.GetApp(ctx, t.AppID)
		if err != nil {
			return err
		}
		apps = []api.App{*app}
	} else if apps, err = c.ListApps(ctx); err != nil {
		return err
	}

	versions := liveVersions(ctx, c, apps)
	if a.jsonOut {
		out := make([]appOut, 0, len(apps))
		for _, app := range apps {
			out = append(out, toAppOut(app, versions[app.ID]))
		}
		return s.P.PrintJSON(struct {
			Apps []appOut `json:"apps"`
		}{out})
	}
	if len(apps) == 0 {
		s.P.Info("There are no apps in %s yet. Create one with: kx deploy", who.Org.ID)
		return nil
	}
	rows := make([][]string, 0, len(apps))
	for _, app := range apps {
		rows = append(rows, []string{app.Name, app.Status, versionText(versions[app.ID]), app.URL})
	}
	s.P.Table([]string{"NAME", "STATUS", "VERSION", "URL"}, rows)
	return nil
}

func runAppsGet(ctx context.Context, a *App, cmd *cobra.Command, name string) error {
	s, err := a.session(cmd, name)
	if err != nil {
		return err
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	c, err := s.Client(ctx)
	if err != nil {
		return err
	}
	version := liveVersions(ctx, c, []api.App{*app})[app.ID]
	if a.jsonOut {
		return s.P.PrintJSON(toAppOut(*app, version))
	}
	rows := [][]string{
		{"Name", app.Name},
		{"ID", app.ID},
		{"Status", app.Status},
		{"Version", versionText(version)},
		{"URL", dashIfEmpty(app.URL)},
		{"Port", strconv.Itoa(app.Port)},
		{"Replicas", strconv.Itoa(app.Replicas)},
	}
	if app.ImageURL != "" {
		rows = append(rows, []string{"Image", app.ImageURL})
	}
	if !app.CreatedAt.IsZero() {
		rows = append(rows, []string{"Created", ui.Date(app.CreatedAt)})
	}
	s.P.Table(nil, rows)
	return nil
}

// liveVersions finds each app's live version, the newest deployment that
// went live (Kubernetix's own release counter only counts some deploys), a
// few apps at a time. A lookup that fails leaves 0.
func liveVersions(ctx context.Context, c *api.Client, apps []api.App) map[string]int {
	out := make(map[string]int, len(apps))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 6)
	for _, app := range apps {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			deps, err := c.ListDeployments(ctx, id, 20)
			if err != nil {
				return
			}
			if live := liveDeployment(deps); live != nil {
				mu.Lock()
				out[id] = live.Version
				mu.Unlock()
			}
		}(app.ID)
	}
	wg.Wait()
	return out
}

func versionText(v int) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("v%d", v)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
