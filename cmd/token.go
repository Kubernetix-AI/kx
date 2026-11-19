package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() { register(newTokenCmd) }

func newTokenCmd(a *App) *cobra.Command {
	tok := &cobra.Command{
		Use:     "token",
		Short:   "List and revoke API tokens",
		GroupID: groupAccount,
		Long: `List and revoke API tokens. Tokens are created in the dashboard, by people
signed in: Settings → API tokens, or for CI, the app's Deploy tab.`,
		Example: `  kx token list
  kx token revoke kx_AbCdEfGh`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	tok.AddCommand(newTokenCreateCmd(), newTokenListCmd(a), newTokenRevokeCmd(a))
	return tok
}

// newTokenCreateCmd is kx token create, which no longer creates tokens:
// only people signed in to the dashboard can. It stays, hidden, so a
// script that still runs it (with any arguments) gets the two lines that
// say where to go, without a request to Kubernetix.
func newTokenCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "create",
		Short:              "Create tokens in the dashboard",
		Long:               "kx can't create tokens; only people signed in to the dashboard can: Settings → API tokens.",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE:               func(*cobra.Command, []string) error { return kxerr.ErrTokenCreateMoved },
	}
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
	c, err := s.Client(ctx)
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
		s.P.Info("There are no tokens in %s. Create one in the dashboard: Settings → API tokens", org)
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
	c, err := s.Client(ctx)
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
