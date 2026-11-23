package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/config"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

// Command groups, in the order --help shows them: what most people need,
// then the rest.
const (
	groupStart = "start"
	groupMore  = "more"
)

// registry holds every command constructor; each command file adds its
// own from init.
var registry []func(*App) *cobra.Command

// register adds a command to kx. Call it from an init function in the
// command's own file:
//
//	func init() { register(newDeployCmd) }
func register(newCmd func(*App) *cobra.Command) { registry = append(registry, newCmd) }

const helpTemplate = `{{with (or .Long .Short)}}{{.}}

{{end}}{{if .HasExample}}Examples:
{{.Example}}

{{end}}Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} <command>{{end}}{{if gt (len .Aliases) 0}}

Also called:
  {{.NameAndAliases}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Commands:{{range $cmds}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) .IsAvailableCommand)}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Options:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Options for every command:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

More about a command: {{.CommandPath}} <command> --help{{end}}
`

func newRoot(a *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "kx",
		Short: "kx puts the app in this folder on Kubernetix.",
		Example: `  kx deploy                build this folder and put it live (logs in first if needed)
  kx status                see what's live
  kx logs --follow         watch your app's output
  kx open                  open your app in the browser`,
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownCommand(cmd, args[0])
			}
			return quickstart(a, cmd)
		},
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	root.SuggestionsMinimumDistance = 2
	cobra.EnableCommandSorting = false
	root.SetHelpTemplate(helpTemplate)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error { return flagError(c, err) })

	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", false, "print machine-readable JSON")
	pf.StringVar(&a.orgFlag, "org", "", "use this `org` (default: the folder's, then your last login)")
	pf.StringVar(&a.apiURL, "api-url", "", "the Kubernetix API `address`")
	_ = pf.MarkHidden("api-url")

	root.AddGroup(
		&cobra.Group{ID: groupStart, Title: "Get started:"},
		&cobra.Group{ID: groupMore, Title: "More commands:"},
	)
	cmds := make([]*cobra.Command, 0, len(registry))
	for _, newCmd := range registry {
		c := newCmd(a)
		c.SilenceErrors, c.SilenceUsage = true, true
		cmds = append(cmds, c)
	}
	sort.SliceStable(cmds, func(i, j int) bool { return commandRank(cmds[i].Name()) < commandRank(cmds[j].Name()) })
	root.AddCommand(cmds...)

	// kx help <command> keeps working, but --help is what kx advertises.
	// Cobra only adds its help command to a command that has subcommands,
	// so this comes after AddCommand.
	root.InitDefaultHelpCmd()
	for _, c := range root.Commands() {
		if c.Name() == "help" {
			c.Hidden = true
		}
	}
	return root
}

// commandOrder is the order --help lists commands in (the order people
// need them); commands not listed follow alphabetically.
var commandOrder = []string{"deploy", "github", "status", "logs", "rollback", "open", "apps", "env", "scale", "link", "login", "logout", "token", "whoami", "version"}

func commandRank(name string) string {
	for i, n := range commandOrder {
		if n == name {
			return fmt.Sprintf("%03d", i)
		}
	}
	return "999" + name
}

func quickstart(a *App, cmd *cobra.Command) error {
	r, err := config.Resolve(config.Flags{Dir: a.Dir}, a.Getenv)
	if err != nil {
		return err
	}
	var orgs []string
	if r.User != nil {
		orgs = r.User.OrgNames()
	}
	p := a.printer()
	if a.jsonOut {
		return p.PrintJSON(struct {
			Steps    []string `json:"steps"`
			LoggedIn []string `json:"logged_in"`
		}{[]string{"kx deploy"}, append([]string{}, orgs...)})
	}
	deploy := "log in, then build this folder and put it live"
	if len(orgs) > 0 {
		deploy = "build this folder and put it live (✓ logged in to " + joinAnd(orgs) + ")"
	}
	p.Result(strings.Join([]string{
		"kx puts the app in this folder on Kubernetix, in two steps:",
		"",
		"  1. Install kx    ✓ done",
		"  2. kx deploy     " + deploy,
		"",
		"Then: kx status, kx logs, kx open. Every command: kx --help",
	}, "\n"))
	return nil
}

func unknownCommand(cmd *cobra.Command, name string) error {
	fix := "See every command: kx --help"
	if s := cmd.SuggestionsFor(name); len(s) > 0 {
		fix = "Did you mean: kx " + s[0]
	}
	return &kxerr.Usage{What: fmt.Sprintf("kx has no command called %q.", name), Fix: fix}
}

var (
	invalidFlagValueRe = regexp.MustCompile(`^invalid argument ("(?:[^"\\]|\\.)*") for "(?:-(\w), )?--([\w-]+)" flag: `)
	shorthandNeedsRe   = regexp.MustCompile(`^flag needs an argument: '(\w)' in -`)
)

// flagError rewrites the flag parser's messages (which can carry Go parse
// errors) as kx's two lines.
func flagError(c *cobra.Command, err error) error {
	msg := err.Error()
	path := c.CommandPath()
	what := strings.TrimSuffix(msg, ".") + "."
	if name, ok := strings.CutPrefix(msg, "unknown flag: "); ok {
		what = fmt.Sprintf("%s doesn't have a %s option.", path, name)
	} else if name, ok := strings.CutPrefix(msg, "unknown shorthand flag: "); ok {
		what = fmt.Sprintf("%s doesn't have a -%s option.", path, strings.SplitN(strings.Trim(name, "'"), "'", 2)[0])
	} else if m := invalidFlagValueRe.FindStringSubmatch(msg); m != nil {
		what = fmt.Sprintf("%s --%s doesn't take %s.", path, m[3], m[1])
	} else if name, ok := strings.CutPrefix(msg, "flag needs an argument: --"); ok {
		what = fmt.Sprintf("%s --%s needs a value.", path, name)
	} else if m := shorthandNeedsRe.FindStringSubmatch(msg); m != nil {
		name := "-" + m[1]
		if f := c.Flags().ShorthandLookup(m[1]); f != nil {
			name = "--" + f.Name
		}
		what = fmt.Sprintf("%s %s needs a value.", path, name)
	}
	return &kxerr.Usage{What: what, Fix: "See examples: " + path + " --help"}
}

// usageArgs returns a cobra.PositionalArgs that allows min..max arguments
// and explains a mismatch in two lines.
func usageArgs(min, max int, use string) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) >= min && len(args) <= max {
			return nil
		}
		what := c.CommandPath() + " takes no arguments."
		switch {
		case max > 0 && len(args) > max:
			what = fmt.Sprintf("%s takes at most %d argument%s.", c.CommandPath(), max, plural(max))
		case len(args) < min:
			what = fmt.Sprintf("%s needs %s.", c.CommandPath(), use)
		}
		return &kxerr.Usage{What: what, Fix: "See examples: " + c.CommandPath() + " --help"}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
