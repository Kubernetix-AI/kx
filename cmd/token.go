package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() { register(newTokenCmd) }

func newTokenCmd(a *App) *cobra.Command {
	tok := &cobra.Command{
		Use:     "token",
		Short:   "Create, list and revoke API tokens",
		GroupID: groupAccount,
		Example: `  kx token create ci --app shop-api
  kx token list
  kx token revoke kx_AbCdEfGh`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	tok.AddCommand(newTokenCreateCmd(a), newTokenListCmd(a), newTokenRevokeCmd(a))
	return tok
}

func newTokenCreateCmd(a *App) *cobra.Command {
	var app, perms, expires string
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a token (shown once)",
		Long: `Create an API token. It is printed once, alone on standard output, so
TOKEN=$(kx token create ci --app shop-api) works.

A token made with kx can't do more or last longer than the login (or
KX_TOKEN) that makes it: by default it can do what that login can and ends
in 90 days, or when the login ends if that's sooner. For longer-lived
tokens, use the dashboard: Settings → API tokens.`,
		Example: `  kx token create                                  every app, until your login ends
  kx token create ci --app shop-api                one app, for CI
  kx token create reader --permissions read --expires 7d`,
		Args: usageArgs(0, 1, "a name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runTokenCreate(cmd.Context(), a, cmd, name, app, perms, expires)
		},
	}
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "limit the token to one `app`")
	f.StringVar(&perms, "permissions", "", "what the token can do, as a `list` of deploy, read, push (default: deploy,read, plus push for every-app tokens, within what your login can do)")
	f.StringVar(&expires, "expires", "", "how long the token lasts, as a `time` from 1d to 365d, and no longer than your login (default: 90d, or when your login ends)")
	return cmd
}

func runTokenCreate(ctx context.Context, a *App, cmd *cobra.Command, name, appFlag, permsFlag, expires string) error {
	if name == "" {
		name = "kx-" + a.Now().UTC().Format("2006-01-02-1504")
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 || strings.TrimSpace(name) != name {
		return &kxerr.Usage{What: "A token's name must be 1 to 64 characters, without spaces at either end.", Fix: "Run: kx token create <name>"}
	}
	days, never, ok := parseExpires(expires)
	if !ok {
		return &kxerr.Usage{What: "kx token create --expires takes 1d to 365d.", Fix: "Run: kx token create --expires 30d"}
	}
	permsSet := cmd.Flags().Changed("permissions")
	var perms []string
	if permsSet {
		if perms, ok = parsePermissions(permsFlag); !ok {
			return &kxerr.Usage{What: "kx token create --permissions takes deploy, read and push, separated by commas.", Fix: "Run: kx token create --permissions deploy,read"}
		}
	}

	s, err := a.session(cmd, appFlag)
	if err != nil {
		return err
	}
	c, err := s.Client()
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	who, err := s.WhoAmI(ctx)
	if err != nil {
		return err
	}
	parent := who.Token // the token making this one
	req := api.CreateTokenRequest{Name: name, ExpiresInDays: days, NeverExpires: never}
	if appFlag != "" || (parent != nil && parent.AppScoped()) {
		app, err := s.ResolveApp(ctx) // an app token's own app
		if err != nil {
			return err
		}
		req.AppID = app.ID
	}
	req.Permissions = perms
	if !permsSet {
		req.Permissions = defaultPermissions(req.AppID != "", parent)
	}
	created, err := c.CreateToken(ctx, org, req)
	if err != nil {
		return explainCreateRefusal(err, parent, req, s.Cfg.TokenSource)
	}
	if a.jsonOut {
		return s.P.SecretJSON(created)
	}
	s.P.Info("Copy this token now. You won't be able to see it again.")
	s.P.Secret(created.Secret)
	s.P.Info("Created %s (%s): %s.", created.Prefix, created.Name, accessText(created.Token))
	return nil
}

func newTokenListCmd(a *App) *cobra.Command {
	var app string
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List tokens",
		Example: "  kx token list\n  kx token list --app shop-api\n  kx token list --json",
		Args:    usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTokenList(cmd.Context(), a, cmd, app)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "only tokens for this `app`")
	return cmd
}

func runTokenList(ctx context.Context, a *App, cmd *cobra.Command, appFlag string) error {
	s, err := a.session(cmd, appFlag)
	if err != nil {
		return err
	}
	c, err := s.Client()
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	appID := ""
	if appFlag != "" {
		app, err := s.ResolveApp(ctx)
		if err != nil {
			return err
		}
		appID = app.ID
	}
	tokens, err := c.ListTokens(ctx, org, appID)
	if err != nil {
		return err
	}
	if a.jsonOut {
		if tokens == nil {
			tokens = []api.Token{}
		}
		return s.P.PrintJSON(struct {
			Tokens []api.Token `json:"tokens"`
		}{tokens})
	}
	if len(tokens) == 0 {
		s.P.Info("There are no tokens in %s. Create one with: kx token create", org)
		return nil
	}
	now := a.Now()
	rows := make([][]string, 0, len(tokens))
	for _, t := range tokens {
		access := "every app"
		if t.AppScoped() {
			access = t.AppName
		}
		expires := "never"
		if t.ExpiresAt != nil {
			expires = ui.Date(*t.ExpiresAt)
		}
		used := "never"
		if t.LastUsedAt != nil {
			used = ui.Ago(*t.LastUsedAt, now)
		}
		rows = append(rows, []string{t.Prefix, t.Name, access, strings.Join(permissionWords(t.Permissions), ", "), expires, used, strings.ReplaceAll(t.Status, "_", " ")})
	}
	s.P.Table([]string{"TOKEN", "NAME", "ACCESS", "CAN", "EXPIRES", "LAST USED", "STATUS"}, rows)
	return nil
}

func newTokenRevokeCmd(a *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke <token>",
		Short: "Revoke a token by its prefix, name or ID",
		Long:  "Revoke a token, and the tokens it created. Anything using it stops working at once.",
		Example: `  kx token revoke kx_AbCdEfGh
  kx token revoke ci --yes`,
		Args: usageArgs(1, 1, "the token's prefix, name or ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTokenRevoke(cmd.Context(), a, cmd, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask first")
	return cmd
}

func runTokenRevoke(ctx context.Context, a *App, cmd *cobra.Command, which string, yes bool) error {
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	c, err := s.Client()
	if err != nil {
		return err
	}
	org, err := s.Org(ctx)
	if err != nil {
		return err
	}
	tokens, err := c.ListTokens(ctx, org, "")
	if err != nil {
		return err
	}
	// A prefix or ID names one token whatever its state; a name can be
	// reused after a token is revoked or expires, so it matches live ones.
	var matches []api.Token
	for _, t := range tokens {
		if t.Prefix == which || t.ID == which || (t.Name == which && t.Status == "active") {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return &kxerr.NoToken{Which: which, Org: org}
	case 1:
	default:
		return &kxerr.Usage{What: fmt.Sprintf("%d tokens match %q.", len(matches), which), Fix: "Revoke one by its prefix (see kx token list): kx token revoke <prefix>"}
	}
	t := matches[0]
	label := fmt.Sprintf("%s (%s)", t.Prefix, t.Name)

	if !yes {
		if !s.P.InTTY {
			return &kxerr.Usage{What: "kx token revoke asks first, and there's no terminal to ask on.", Fix: "Add --yes: kx token revoke " + which + " --yes"}
		}
		ok, err := s.P.Confirm(ctx, "Revoke "+label+"? Anything using it stops working.")
		if err != nil {
			return err
		}
		if !ok {
			s.P.Info("Nothing revoked.")
			return nil
		}
	}
	if err := c.RevokeToken(ctx, org, t.ID); err != nil {
		return err
	}
	if a.jsonOut {
		return s.P.PrintJSON(struct {
			Revoked string `json:"revoked"`
			ID      string `json:"id"`
		}{t.Prefix, t.ID})
	}
	s.P.Info("Revoked %s.", label)
	if s.Cfg.User != nil {
		if l, ok := s.Cfg.User.Orgs[org]; ok && api.TokenPrefix(l.Token) == t.Prefix {
			s.P.Info("This computer was using that token for %s; run kx login to log in again.", org)
		}
	}
	if s.Cfg.TokenSource == kxerr.SourceEnv && api.TokenPrefix(s.Cfg.Token) == t.Prefix {
		s.P.Info("KX_TOKEN holds that token, so it stops working now.")
	}
	return nil
}

// accessText describes a token: "deploy, read on shop-api until 10 Feb 2027".
func accessText(t api.Token) string {
	scope := "every app"
	if t.AppScoped() {
		scope = t.AppName
	}
	until := "with no end date"
	if t.ExpiresAt != nil {
		until = "until " + ui.Date(*t.ExpiresAt)
	}
	return strings.Join(permissionWords(t.Permissions), ", ") + " on " + scope + " " + until
}

// parseExpires reads --expires: "" (server default, 90 days), 1d-365d, 1y
// or never.
func parseExpires(s string) (days int, never, ok bool) {
	switch s {
	case "":
		return 0, false, true
	case "never":
		return 0, true, true
	case "1y":
		return 365, false, true
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || !strings.HasSuffix(s, "d") || n < 1 || n > 365 {
		return 0, false, false
	}
	return n, false, true
}

// defaultPermissions is what a new token gets without --permissions:
// deploy and read, plus registry:push for tokens that reach every app,
// narrowed to what parent (the token making it) can do.
func defaultPermissions(oneApp bool, parent *api.Token) []string {
	want := []string{api.PermDeploy, api.PermRead}
	if !oneApp {
		want = append(want, api.PermPush)
	}
	if parent == nil {
		return want
	}
	var out []string
	for _, p := range want {
		if parent.Can(p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = append(out, parent.Permissions...)
	}
	return out
}

// explainCreateRefusal turns the platform's token_not_allowed answer to a
// token create into what kx can say about it. A token can only make tokens
// narrower than itself, and kx knows both the token and the request, so it
// can tell which limit was hit. Any other error is returned as it is.
func explainCreateRefusal(err error, parent *api.Token, req api.CreateTokenRequest, src kxerr.TokenSource) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "token_not_allowed" || parent == nil {
		return err
	}
	r := &kxerr.TokenCreateRefused{Source: src, Until: parent.ExpiresAt, Permissions: parent.Permissions, App: parent.AppName}
	switch {
	case parent.ParentTokenID != "" || parent.CreatedVia == "token":
		r.Reason = kxerr.CreateDerived
	case !withinPermissions(req.Permissions, parent):
		r.Reason = kxerr.CreatePermissions
	case parent.AppScoped() && req.AppID != parent.AppID:
		r.Reason = kxerr.CreateApp
	case req.NeverExpires || (req.ExpiresInDays > 0 && parent.ExpiresAt != nil):
		// What's left is the expiry: never, or past the token's own end.
		r.Reason = kxerr.CreateExpiry
	default:
		return err
	}
	return r
}

func withinPermissions(perms []string, t *api.Token) bool {
	for _, p := range perms {
		if !t.Can(p) {
			return false
		}
	}
	return true
}

// parsePermissions reads --permissions: deploy, read and push, separated
// by commas.
func parsePermissions(s string) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "push" {
			p = api.PermPush
		}
		switch p {
		case api.PermDeploy, api.PermRead, api.PermPush:
		default:
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, len(out) > 0
}
