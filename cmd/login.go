package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/ui"
)

func init() {
	register(newLoginCmd)
	register(newLogoutCmd)
	register(newWhoamiCmd)
}

func newLoginCmd(a *App) *cobra.Command {
	var expires string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:     "login",
		Short:   "Connect this computer to Kubernetix",
		GroupID: groupStart,
		Long: `Log in by creating a token for this computer in your browser and pasting it here.
Logging in to another organization adds it beside the first.`,
		Example: `  kx login                          open the dashboard, then paste the token
  kx login --org acme-labs          log in to another organization
  kx login --expires 1d             make a token that lasts one day
  echo "$TOKEN" | kx login --token-stdin`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd.Context(), a, cmd, expires, fromStdin)
		},
	}
	cmd.Flags().StringVar(&expires, "expires", "", "make the new token last this `time`: 1d (default 30 days)")
	cmd.Flags().BoolVar(&fromStdin, "token-stdin", false, "read the token from standard input instead of asking")
	return cmd
}

func runLogin(ctx context.Context, a *App, cmd *cobra.Command, expires string, fromStdin bool) error {
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	if s.CI.IsCI {
		return kxerr.ErrLoginInCI
	}
	switch expires {
	case "", "30d", "1d":
	default:
		return &kxerr.Usage{What: "kx login --expires takes 1d (the default is 30 days).", Fix: "Run: kx login --expires 1d"}
	}
	if a.orgFlag != "" && !config.ValidOrgName(a.orgFlag) {
		return &kxerr.Usage{What: fmt.Sprintf("%q isn't an organization name.", a.orgFlag), Fix: "Check the name, or see where you're logged in: kx whoami"}
	}
	p := s.P

	var tok string
	if fromStdin {
		if tok, err = p.ReadLine(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &kxerr.InvalidToken{Source: kxerr.SourcePaste, Reason: "nothing on standard input"}
		}
	} else {
		if !p.InTTY {
			return &kxerr.Usage{What: "kx login needs a terminal to paste the token into.", Fix: "Pipe the token in instead: kx login --token-stdin"}
		}
		page := tokenPageURL(s, a.orgFlag, expires)
		if a.CanOpenBrowser() && a.OpenBrowser(page) == nil {
			p.Info("Opening %s so you can create a token for this computer.", s.DashboardHost())
			p.Info("If your browser didn't open, go to %s", page)
		} else {
			p.Info("Open this page so you can create a token for this computer:")
			p.Info("%s", page)
		}
		p.Info(`Click "Create token", then "Copy", and paste it here.`)
		if tok, err = p.ReadSecret(ctx, "Token: "); err != nil {
			if errors.Is(err, ui.ErrNoTTY) {
				return &kxerr.Usage{What: "kx login needs a terminal to paste the token into.", Fix: "Pipe the token in instead: kx login --token-stdin"}
			}
			return err
		}
	}

	if err := api.CheckToken(tok); err != nil {
		var inv *kxerr.InvalidToken
		if errors.As(err, &inv) {
			inv.Source = kxerr.SourcePaste
		}
		return err
	}
	c := a.newClient(s.Cfg.APIURL, tok, s.CI)
	c.Ctx = api.ErrContext{TokenSource: kxerr.SourcePaste}
	who, err := c.WhoAmI(ctx)
	if err != nil {
		return err
	}
	org := who.Org.ID
	if !config.ValidOrgName(org) {
		return errors.New("the server named an organization kx cannot save")
	}

	login := config.OrgLogin{Token: tok, User: who.User.Email}
	if who.Token != nil {
		login.ExpiresAt = who.Token.ExpiresAt
	}
	prev, hadPrev := s.Cfg.User.Orgs[org]
	s.Cfg.User.AddLogin(org, login)
	if err := config.SaveUser(s.Cfg.UserPath, s.Cfg.User); err != nil {
		return fmt.Errorf("saving the login: %w", err)
	}

	if a.orgFlag != "" && a.orgFlag != org {
		p.Warn("This token is for %s, not %s, so kx saved it for %s.", org, a.orgFlag, org)
	}
	if s.Cfg.TokenSource == kxerr.SourceEnv {
		p.Warn("KX_TOKEN is set, so kx keeps using it instead of this login.")
	}
	var previous *previousOut
	if hadPrev && api.TokenPrefix(prev.Token) != api.TokenPrefix(tok) && (prev.ExpiresAt == nil || a.Now().Before(*prev.ExpiresAt)) {
		res := retirePreviousToken(ctx, a, s, org, prev.Token)
		previous = &previousOut{TokenPrefix: api.TokenPrefix(prev.Token), Revoked: res.revoked}
		switch {
		case a.jsonOut || res.line == "":
		case res.warn:
			p.Warn("%s", res.line)
		default:
			p.Info("%s", res.line)
		}
	}
	if a.jsonOut {
		out := loginJSON(org, who)
		out.PreviousToken = previous
		return p.PrintJSON(out)
	}
	as := ""
	if who.User.Email != "" {
		as = " as " + who.User.Email
	}
	p.Info("Logged in to %s%s. %s", org, as, tokenSentence(who.Token, org))
	p.Info("Next: open your project folder and run kx deploy")
	return nil
}

func tokenPageURL(s *Session, org, expires string) string {
	page := s.DashboardURL("/settings?tab=tokens&new=cli")
	if org != "" {
		page += "&org=" + url.QueryEscape(org)
	}
	if expires == "1d" {
		page += "&expires=1d"
	}
	return page
}

// tokenSentence says what the token can do and until when:
// "This token can deploy every app in acme-corp until 11 Dec 2026."
func tokenSentence(t *api.Token, org string) string {
	if t == nil {
		return ""
	}
	verb := "read"
	if t.Can(api.PermDeploy) {
		verb = "deploy"
	}
	what := "every app in " + org
	if t.AppScoped() {
		what = t.AppName
	}
	until := "and never expires"
	if t.ExpiresAt != nil {
		until = "until " + ui.Date(*t.ExpiresAt)
	}
	return fmt.Sprintf("This token can %s %s %s.", verb, what, until)
}

type loginOut struct {
	Org           string       `json:"org"`
	User          string       `json:"user,omitempty"`
	TokenPrefix   string       `json:"token_prefix,omitempty"`
	Scope         string       `json:"scope,omitempty"`
	App           string       `json:"app,omitempty"`
	Permissions   []string     `json:"permissions,omitempty"`
	ExpiresAt     *time.Time   `json:"expires_at"`
	PreviousToken *previousOut `json:"previous_token,omitempty"`
}

// previousOut is the token a new login replaced, and whether kx revoked it.
type previousOut struct {
	TokenPrefix string `json:"token_prefix"`
	Revoked     bool   `json:"revoked"`
}

func loginJSON(org string, who *api.WhoAmI) loginOut {
	out := loginOut{Org: org, User: who.User.Email}
	if t := who.Token; t != nil {
		out.TokenPrefix, out.Scope, out.App, out.Permissions, out.ExpiresAt = t.Prefix, t.Scope, t.AppName, t.Permissions, t.ExpiresAt
	}
	return out
}

func newLogoutCmd(a *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "logout",
		Short:   "Revoke this computer's token and forget it",
		GroupID: groupAccount,
		Long: `Revoke the saved token on Kubernetix, then delete it from this computer.

Revoking a token also revokes the tokens made with it (kx token create). If
there are any, kx lists them and asks first; without a terminal it needs
--yes, and otherwise logs out on this computer only.`,
		Example: `  kx logout                  log out of your current organization
  kx logout --org acme-labs  log out of another one
  kx logout --yes            also revoke tokens made with this login, without asking`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogout(cmd.Context(), a, cmd, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "also revoke the tokens made with this login, without asking")
	return cmd
}

// madeOut is a token made with the login being logged out, in --json.
type madeOut struct {
	TokenPrefix string `json:"token_prefix"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	App         string `json:"app,omitempty"`
}

func runLogout(ctx context.Context, a *App, cmd *cobra.Command, yes bool) error {
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	if s.Cfg.User == nil {
		return kxerr.ErrNotLoggedIn
	}
	org := a.orgFlag
	if org == "" {
		org = s.Cfg.User.DefaultOrg
	}
	login, ok := s.Cfg.User.Orgs[org]
	if !ok {
		if others := s.Cfg.User.OrgNames(); len(others) > 0 && org != "" {
			return &kxerr.OrgNotLoggedIn{Org: org, LoggedIn: others}
		}
		return kxerr.ErrNotLoggedIn
	}
	p := s.P
	prefix := api.TokenPrefix(login.Token)

	result := tokenDead
	var own *ownToken
	asked := false
	if login.ExpiresAt == nil || a.Now().Before(*login.ExpiresAt) {
		if own, err = inspectOwnToken(ctx, a, s, org, login.Token); err != nil {
			return err
		}
		result = own.state
		if result == tokenLive {
			cascade := len(own.children) == 0 && !own.unknown
			if !cascade && yes {
				cascade = true
			} else if !cascade && p.InTTY {
				asked = true
				if cascade, err = confirmCascade(ctx, p, own); err != nil {
					return err
				}
			}
			result = tokenKept
			if cascade {
				if result, err = own.revoke(ctx); err != nil {
					return err
				}
			}
		}
	}
	s.Cfg.User.RemoveLogin(org)
	if err := config.SaveUser(s.Cfg.UserPath, s.Cfg.User); err != nil {
		return fmt.Errorf("saving the logout: %w", err)
	}

	var children []api.Token
	unknown := false
	if own != nil {
		children, unknown = own.children, own.unknown
	}
	if a.jsonOut {
		made := make([]madeOut, 0, len(children))
		for _, t := range children {
			made = append(made, madeOut{TokenPrefix: t.Prefix, Name: t.Name, Scope: t.Scope, App: t.AppName})
		}
		return p.PrintJSON(struct {
			LoggedOut     string    `json:"logged_out"`
			Revoked       bool      `json:"revoked"`        // with the tokens made with it
			KeptOnServer  bool      `json:"kept_on_server"` // still works, for the tokens made with it
			NeedsRevoking bool      `json:"needs_revoking"` // refused by Kubernetix: revoke it in the dashboard
			MadeWithIt    []madeOut `json:"tokens_made_with_it"`
			MadeUnknown   bool      `json:"tokens_made_with_it_unknown"`
		}{org, result == tokenRevoked, result == tokenKept, result == tokenRefused, made, unknown})
	}

	n := len(children)
	made := fmt.Sprintf("the %d %s made with it", n, plural2(n, "token", "tokens"))
	switch {
	case result == tokenRevoked && n > 0:
		p.Info("Logged out of %s and revoked its token (%s) and %s.", org, prefix, made)
	case result == tokenRevoked && unknown:
		p.Info("Logged out of %s and revoked its token (%s) and any tokens made with it.", org, prefix)
	case result == tokenRevoked:
		p.Info("Logged out of %s and revoked its token (%s).", org, prefix)
	case result == tokenRefused:
		p.Info("Logged out of %s, but kx couldn't revoke its token (%s) because Kubernetix refused it; revoke it in Settings → API tokens.", org, prefix)
	case result == tokenKept && asked && n > 0:
		p.Info("Logged out of %s on this computer only; its token (%s) still works on Kubernetix, so %s %s working.", org, prefix, made, plural2(n, "keeps", "keep"))
	case result == tokenKept && asked:
		p.Info("Logged out of %s on this computer only; its token (%s) still works on Kubernetix.", org, prefix)
	case result == tokenKept && n > 0:
		p.Info("Logged out of %s on this computer only; its token (%s) still works on Kubernetix, because revoking it would also revoke %s.", org, prefix, made)
		p.Info("To revoke them too, revoke %s in the dashboard (Settings → API tokens), or use kx logout --yes next time.", prefix)
	case result == tokenKept:
		p.Info("Logged out of %s on this computer only; its token (%s) still works on Kubernetix, because kx couldn't check for tokens made with it, which revoking it would also revoke.", org, prefix)
		p.Info("To revoke it, use the dashboard (Settings → API tokens), or kx logout --yes next time.")
	default:
		p.Info("Logged out of %s (its token had already stopped working).", org)
	}
	if rest := s.Cfg.User.OrgNames(); len(rest) > 0 {
		p.Info("Still logged in to %s.", joinAnd(rest))
	}
	return nil
}

// confirmCascade shows the tokens made with own (or that kx couldn't
// check) and asks whether to revoke them with it. The default is no.
func confirmCascade(ctx context.Context, p *ui.Printer, own *ownToken) (bool, error) {
	if own.unknown {
		p.Info("kx can't see which tokens were made with this login; revoking it also revokes them.")
		return p.Confirm(ctx, "Revoke it anyway?")
	}
	n := len(own.children)
	if n == 1 {
		p.Info("This token was made with this login and stops working if it's revoked:")
	} else {
		p.Info("These %d tokens were made with this login and stop working if it's revoked:", n)
	}
	width := 0
	for _, t := range own.children {
		width = max(width, len(t.Name))
	}
	for _, t := range own.children {
		access := "every app"
		if t.AppScoped() {
			access = t.AppName
		}
		p.Note("%s  %-*s  %s", t.Prefix, width, t.Name, access)
	}
	if n == 1 {
		return p.Confirm(ctx, "Also revoke this token?")
	}
	return p.Confirm(ctx, fmt.Sprintf("Also revoke these %d tokens?", n))
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// revokeResult is what kx found out about, or did with, a token it saved.
type revokeResult int

const (
	tokenRevoked revokeResult = iota // revoked just now
	tokenDead                        // already unusable (expired, revoked, unknown)
	tokenRefused                     // refused because of its creator, so it can't revoke itself
	tokenLive                        // works; not revoked yet
	tokenKept                        // works and was left working on purpose
)

// ownToken is a token kx saved, as seen with the token itself: whether it
// works and, if so, the active tokens made with it. Revoking it revokes
// those too. unknown means the token can't list tokens (no read
// permission), so kx can't tell.
type ownToken struct {
	state    revokeResult
	children []api.Token
	unknown  bool

	client *api.Client
	org    string
	id     string
}

// inspectOwnToken looks tok (the token saved for org) up with tok itself.
// A token that no longer works is tokenDead; one the platform refuses
// because of its creator (awaiting approval, not approved) is tokenRefused.
// Anything else (offline, server error) is an error.
func inspectOwnToken(ctx context.Context, a *App, s *Session, org, tok string) (*ownToken, error) {
	if api.CheckToken(tok) != nil {
		return &ownToken{state: tokenDead}, nil
	}
	c := a.newClient(s.Cfg.APIURL, tok, s.CI)
	c.Ctx = api.ErrContext{Org: org, TokenSource: kxerr.SourceLogin}
	who, err := c.WhoAmI(ctx)
	if err != nil {
		state, err := revokeFailure(err)
		return &ownToken{state: state}, err
	}
	if who.Token == nil {
		return &ownToken{state: tokenDead}, nil
	}
	o := &ownToken{state: tokenLive, client: c, org: who.Org.ID, id: who.Token.ID}
	tokens, err := c.ListTokens(ctx, who.Org.ID, "")
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Status == 403 && apiErr.Code == "token_not_allowed" {
			o.unknown = true
			return o, nil
		}
		state, err := revokeFailure(err)
		return &ownToken{state: state}, err
	}
	for _, t := range tokens {
		if t.ParentTokenID == o.id && t.Status == "active" {
			o.children = append(o.children, t)
		}
	}
	return o, nil
}

// revoke revokes a live token (and so the tokens made with it).
func (o *ownToken) revoke(ctx context.Context) (revokeResult, error) {
	if err := o.client.RevokeToken(ctx, o.org, o.id); err != nil {
		return revokeFailure(err)
	}
	return tokenRevoked, nil
}

// revokeFailure sorts an error from using a saved token: a 401 means the
// token no longer works, a 403 token_owner_* that the platform refuses it
// because of its creator; anything else is returned.
func revokeFailure(err error) (revokeResult, error) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 401:
			return tokenDead, nil
		case apiErr.Status == 403 && strings.HasPrefix(apiErr.Code, "token_owner_"):
			return tokenRefused, nil
		}
	}
	return 0, err
}

// retired is the outcome of retirePreviousToken: the line to print (empty
// when there is nothing to say), whether it is a warning, and whether the
// token was revoked.
type retired struct {
	line    string
	warn    bool
	revoked bool
}

// retirePreviousToken revokes old, the token this computer used for org
// before a new login replaced it, so it isn't left active and forgotten.
// Revoking a token also revokes the tokens made with it (a CI token made
// with kx token create, say), so a token that has any, or that can't tell,
// is kept. It is best effort and never fails the new login.
func retirePreviousToken(ctx context.Context, a *App, s *Session, org, old string) retired {
	prefix := api.TokenPrefix(old)
	failed := retired{line: fmt.Sprintf("kx couldn't revoke the token this computer used before for %s (%s); revoke it with: kx token revoke %s", org, prefix, prefix), warn: true}
	own, err := inspectOwnToken(ctx, a, s, org, old)
	switch {
	case err != nil || own.state == tokenRefused || own.unknown:
		return failed
	case own.state != tokenLive:
		return retired{}
	case len(own.children) > 0:
		n := len(own.children)
		return retired{line: fmt.Sprintf("The token this computer used before for %s (%s) still works, because %d %s on it; to revoke them all: kx token revoke %s",
			org, prefix, n, plural2(n, "token made with it depends", "tokens made with it depend"), prefix)}
	}
	state, err := own.revoke(ctx)
	switch {
	case err != nil || state == tokenRefused:
		return failed
	case state != tokenRevoked:
		return retired{}
	}
	return retired{line: fmt.Sprintf("Revoked the token this computer used before for %s (%s).", org, prefix), revoked: true}
}

func newWhoamiCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "whoami",
		Short:   "Show where this computer is logged in",
		GroupID: groupAccount,
		Example: `  kx whoami
  kx whoami --json`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWhoami(cmd.Context(), a, cmd)
		},
	}
}

// whoamiOrg is one entry of kx whoami --json, the same for a saved login
// (source "login") and for KX_TOKEN (source "env").
type whoamiOrg struct {
	Org         string     `json:"org"`
	User        string     `json:"user"`
	TokenPrefix string     `json:"token_prefix"`
	ExpiresAt   *time.Time `json:"expires_at"`
	Ended       bool       `json:"ended"`
	Default     bool       `json:"default"` // the org kx uses when none is named
	Source      string     `json:"source"`
}

type whoamiOut struct {
	Orgs []whoamiOrg `json:"orgs"`
}

func runWhoami(ctx context.Context, a *App, cmd *cobra.Command) error {
	s, err := a.session(cmd, "")
	if err != nil {
		return err
	}
	p := s.P
	if s.Cfg.TokenSource == kxerr.SourceEnv {
		who, err := s.WhoAmI(ctx)
		if err != nil {
			return err
		}
		if a.jsonOut {
			e := whoamiOrg{Org: who.Org.ID, User: who.User.Email, Default: true, Source: "env"}
			if t := who.Token; t != nil {
				e.TokenPrefix, e.ExpiresAt = t.Prefix, t.ExpiresAt
				e.Ended = t.ExpiresAt != nil && !a.Now().Before(*t.ExpiresAt)
			}
			return p.PrintJSON(whoamiOut{[]whoamiOrg{e}})
		}
		ends, access, label := "never", "", ""
		if t := who.Token; t != nil {
			if t.ExpiresAt != nil {
				ends = ui.Date(*t.ExpiresAt)
			}
			scope := "every app"
			if t.AppScoped() {
				scope = t.AppName
			}
			access = strings.Join(permissionWords(t.Permissions), ", ") + " on " + scope
			label = fmt.Sprintf("%s (%s)", t.Prefix, t.Name)
		}
		p.Table([]string{"ORG", "USER", "TOKEN", "ACCESS", "ENDS"}, [][]string{{who.Org.ID, who.User.Email, label, access, ends}})
		return nil
	}

	if s.Cfg.User == nil || len(s.Cfg.User.Orgs) == 0 {
		return kxerr.ErrNotLoggedIn
	}
	now := a.Now()
	var rows [][]string
	var out []whoamiOrg
	for _, org := range s.Cfg.User.OrgNames() {
		l := s.Cfg.User.Orgs[org]
		ends := "never"
		ended := l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
		if l.ExpiresAt != nil {
			ends = ui.Date(*l.ExpiresAt)
			if ended {
				ends = "ended " + ends
			}
		}
		def := org == s.Cfg.User.DefaultOrg
		if def {
			ends += " (default)"
		}
		rows = append(rows, []string{org, l.User, ends})
		out = append(out, whoamiOrg{Org: org, User: l.User, TokenPrefix: api.TokenPrefix(l.Token), ExpiresAt: l.ExpiresAt, Ended: ended, Default: def, Source: "login"})
	}
	if a.jsonOut {
		return p.PrintJSON(whoamiOut{out})
	}
	p.Table([]string{"ORG", "USER", "LOGIN ENDS"}, rows)
	return nil
}

// permissionWords turns API permissions into the words kx shows.
func permissionWords(perms []string) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		if p == api.PermPush {
			p = "push"
		}
		out = append(out, p)
	}
	return out
}
