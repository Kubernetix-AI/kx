package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		GroupID: groupMore,
		Long: `Save the app this folder deploys to in .kx.json, so later commands need no --app.
.kx.json holds no secret, so you can commit it for your team and CI.`,
		Example: `  kx link              pick an app, or a new one named after the folder
  kx link shop-api     link to shop-api (a new app if there's none by that name)`,
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
// name of a new one that kx deploy will create. unasked means kx chose it
// without a question, because there was nothing to choose from.
type linkChoice struct {
	app     *api.App
	newApp  string
	unasked bool
}

func runLink(ctx context.Context, a *App, cmd *cobra.Command, name string) error {
	if name != "" && !config.ValidAppName(name) {
		return &kxerr.Usage{What: fmt.Sprintf("%q isn't an app name; app names look like shop-api.", name), Fix: "See your apps: kx apps list"}
	}
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	choice, err := chooseApp(ctx, s, name, "link", s.P.InTTY && s.P.TTY)
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
// deploy every app, becomes a new app. With no name and no apps yet, a
// token that can create apps gets a new app named after the folder,
// without a question, unless the folder's name says nothing about the app
// (newAppName). Otherwise kx asks. For a token that can create apps, the
// first choice, and the preselected one, is a new app named after the
// folder ("(new app: my-app)"). An existing app is never preselected, even
// one with the folder's name (it is listed first): deploying a folder over
// an app takes an explicit pick. command is the command asking ("link" or
// "deploy"), for the fix lines; canAsk says whether kx may ask.
func chooseApp(ctx context.Context, s *Session, name, command string, canAsk bool) (linkChoice, error) {
	c, err := s.Client(ctx)
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
	suggested := config.AppNameFrom(filepath.Base(s.Cfg.Dir))
	if len(apps) == 0 && canCreate && suggested != "" {
		return newAppName(ctx, s, suggested, command, canAsk)
	}
	var labels []string
	var choices []linkChoice
	def := -1
	if canCreate && suggested != "" && !hasApp(apps, suggested) {
		labels = append(labels, "(new app: "+suggested+")")
		choices = append(choices, linkChoice{newApp: suggested})
		def = 0
	}
	// The app named like the folder first, then the rest as listed.
	for _, first := range []bool{true, false} {
		for i := range apps {
			if (apps[i].Name == suggested) == first {
				labels = append(labels, apps[i].Name)
				choices = append(choices, linkChoice{app: &apps[i]})
			}
		}
	}
	nameIt := "Name the new app: kx link <name>"
	if command == "deploy" {
		nameIt += ", then run kx deploy"
	}
	switch {
	case len(choices) == 0 && canCreate:
		return linkChoice{}, &kxerr.Usage{What: "There are no apps yet, and kx can't make an app name from this folder's name.", Fix: nameIt}
	case len(choices) == 0:
		return linkChoice{}, &kxerr.Usage{
			What: "There are no apps to link to yet.",
			Fix:  "Create an app in the dashboard, or log in with a token that can deploy: kx login",
		}
	}
	var i int
	if canAsk {
		i, err = s.P.Choose(ctx, "Which app should this folder deploy to?", labels, def)
	} else {
		err = ui.ErrNoTTY
	}
	switch {
	case errors.Is(err, ui.ErrNoTTY) && command == "deploy":
		return linkChoice{}, &kxerr.Usage{What: "This folder isn't linked to an app yet, and kx can't ask which one here.", Fix: "Run: kx deploy --app <app>"}
	case errors.Is(err, ui.ErrNoTTY):
		return linkChoice{}, &kxerr.Usage{What: "kx link needs an app name when it can't ask.", Fix: "Run: kx link <app>"}
	case errors.Is(err, ui.ErrNoChoice):
		fix := "Run kx link again and choose an app by its number, or name it: kx link <app>"
		if command == "deploy" {
			fix = "Run kx deploy again and choose an app by its number, or name it: kx deploy --app <app>"
		}
		return linkChoice{}, &kxerr.Usage{What: "No app was chosen, so this folder isn't linked yet.", Fix: fix}
	case err != nil:
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

// newAppName names the first app of an organization that has none:
// suggested, the folder's name made into an app name, without a question.
// A folder whose name says nothing about the app (genericFolder) asks for
// a name instead, suggesting the git repository's; without a terminal
// that's a usage error.
func newAppName(ctx context.Context, s *Session, suggested, command string, canAsk bool) (linkChoice, error) {
	if !genericFolder(s.Cfg.Dir, s.App.Getenv) {
		return linkChoice{newApp: suggested, unasked: true}, nil
	}
	needName := &kxerr.Usage{What: "kx needs an app name for this folder.", Fix: "Run: kx deploy --app <name>"}
	if command == "link" {
		needName.Fix = "Run: kx link <name>"
	}
	if !canAsk {
		return linkChoice{}, needName
	}
	if origin := gitOriginName(s.Cfg.Dir); origin != "" {
		suggested = origin
	}
	for {
		name, err := s.P.Ask(ctx, "What should the app be called?", suggested)
		switch {
		case errors.Is(err, ui.ErrNoChoice):
			return linkChoice{}, needName
		case err != nil:
			return linkChoice{}, err
		case config.ValidAppName(name):
			return linkChoice{newApp: name}, nil
		}
		s.P.Info("App names use lowercase letters, numbers and hyphens, like shop-api. Try again.")
	}
}

// genericFolderNames are folder names that say nothing about the app in
// the folder, so kx doesn't name a new app after them without asking.
var genericFolderNames = map[string]bool{
	"src": true, "app": true, "apps": true, "code": true, "project": true, "projects": true,
	"repo": true, "repos": true, "dev": true, "work": true, "workspace": true, "web": true,
	"site": true, "www": true, "dist": true, "build": true, "desktop": true, "documents": true,
	"downloads": true, "home": true, "tmp": true, "temp": true, "test": true, "untitled": true,
	"new-folder": true, "html": true, "public": true, "users": true, "source": true, "git": true,
	"github": true, "go": true,
}

// genericFolder reports whether dir's name says nothing about the app in
// it (src, app, Desktop, New folder…), or dir is the home folder.
func genericFolder(dir string, getenv func(string) string) bool {
	base := filepath.Base(dir)
	if genericFolderNames[strings.ToLower(base)] || genericFolderNames[config.AppNameFrom(base)] {
		return true
	}
	here, err := os.Stat(dir)
	if err != nil {
		return false
	}
	homes := []string{getenv("HOME"), getenv("USERPROFILE")}
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, h)
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		if fi, err := os.Stat(h); err == nil && os.SameFile(here, fi) {
			return true
		}
	}
	return false
}

// gitOriginName is the repository's name in the address of the git remote
// called origin, made into an app name ("shop-api" for
// https://github.com/acme/shop-api.git), for the repository dir is in.
// It is "" when there is no repository, no origin, or no usable name.
func gitOriginName(dir string) string {
	cfg := gitConfigPath(dir)
	if cfg == "" {
		return ""
	}
	f, err := os.Open(cfg) //nolint:gosec // G304: the repository's own git config
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin := false
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = strings.EqualFold(strings.Join(strings.Fields(line), " "), `[remote "origin"]`)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !inOrigin || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		u := strings.Trim(strings.TrimSpace(value), `"`)
		u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
		if i := strings.LastIndexAny(u, "/:"); i >= 0 {
			u = u[i+1:]
		}
		return config.AppNameFrom(u)
	}
	return ""
}

// gitConfigPath finds the git config of the repository dir is in: the
// .git folder's config, or for a worktree (whose .git is a file naming its
// folder in the main repository) the main repository's. "" if none.
func gitConfigPath(dir string) string {
	for d := filepath.Clean(dir); ; {
		dotGit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotGit)
		switch {
		case err == nil && fi.IsDir():
			return filepath.Join(dotGit, "config")
		case err == nil:
			b, err := os.ReadFile(dotGit)
			if err != nil {
				return ""
			}
			gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
			if !ok {
				return ""
			}
			gitDir = strings.TrimSpace(gitDir)
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(d, gitDir)
			}
			if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil { //nolint:gosec // G304: as above
				c := strings.TrimSpace(string(common))
				if !filepath.IsAbs(c) {
					c = filepath.Join(gitDir, c)
				}
				gitDir = c
			}
			return filepath.Join(gitDir, "config")
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}
