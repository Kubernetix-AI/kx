package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
)

func init() { register(newEnvCmd) }

func newEnvCmd(a *App) *cobra.Command {
	var listApp string
	list := &cobra.Command{
		Use:     "list",
		Short:   "List the app's environment variables (names only)",
		Example: "  kx env list\n  kx env list --app shop-api --json",
		Args:    usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvList(cmd.Context(), a, cmd, listApp)
		},
	}
	list.Flags().StringVar(&listApp, "app", "", "the `app` (default: the one this folder is linked to)")

	var setFlags configureFlags
	var fromFile string
	set := &cobra.Command{
		Use:   "set KEY=VALUE...",
		Short: "Set environment variables, then deploy the app",
		Long: `Set (add or change) environment variables of the app, keep the others, then deploy the app
as configured and wait until it's live, like kx deploy. kx never prints a value.

A KEY with no =VALUE takes its value from kx's own environment, so a CI secret never
appears on the command line. --from-file reads KEY=VALUE lines from a file such as .env.`,
		Example: `  kx env set LOG_LEVEL=debug FEATURE_X=on
  kx env set STRIPE_KEY                     the value of $STRIPE_KEY
  kx env set --from-file .env.production
  kx env set LOG_LEVEL=info --no-deploy     save it; the next deploy applies it`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvSet(cmd.Context(), a, cmd, setFlags, fromFile, args)
		},
	}
	addConfigureFlags(set, &setFlags)
	set.Flags().StringVar(&fromFile, "from-file", "", "read KEY=VALUE lines from this `file` (such as .env)")

	var unsetFlags configureFlags
	unset := &cobra.Command{
		Use:     "unset KEY...",
		Short:   "Remove environment variables, then deploy the app",
		Example: "  kx env unset OLD_FLAG\n  kx env unset OLD_FLAG LEGACY_URL --no-deploy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvUnset(cmd.Context(), a, cmd, unsetFlags, args)
		},
	}
	addConfigureFlags(unset, &unsetFlags)

	env := &cobra.Command{
		Use:     "env",
		Short:   "See and change the app's environment variables",
		GroupID: groupMore,
		Example: `  kx env                         list the names
  kx env set LOG_LEVEL=debug     set one, then deploy
  kx env unset OLD_FLAG          remove one, then deploy`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvList(cmd.Context(), a, cmd, listApp)
		},
	}
	env.Flags().StringVar(&listApp, "app", "", "the `app` (default: the one this folder is linked to)")
	env.AddCommand(list, set, unset)
	return env
}

func runEnvList(ctx context.Context, a *App, cmd *cobra.Command, appFlag string) error {
	s, err := a.session(cmd, appFlag)
	if err != nil {
		return err
	}
	if _, err := s.Client(ctx); err != nil {
		return err
	}
	if err := requirePermissions(ctx, s, "kx env", api.PermRead); err != nil {
		return err
	}
	app, err := s.ResolveApp(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(app.EnvVars))
	for k := range app.EnvVars {
		names = append(names, k)
	}
	sort.Strings(names)
	if a.jsonOut {
		return s.P.PrintJSON(struct {
			App          string   `json:"app"`
			Names        []string `json:"names"`
			ValuesHidden bool     `json:"values_hidden"`
		}{app.Name, names, true})
	}
	if len(names) == 0 {
		s.P.Info("%s has no environment variables.", app.Name)
		return nil
	}
	for _, n := range names {
		s.P.Result(n)
	}
	s.P.Info("Values are hidden: kx only sees the names.")
	return nil
}

func runEnvSet(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, fromFile string, args []string) error {
	values, err := envValues(a, fromFile, args)
	if err != nil {
		return err
	}
	names := sortedKeys(values)
	return runConfigure(ctx, a, cmd, f, appChange{
		command: "kx env",
		step:    func(app *api.App) (string, string) { return "Set", listOf(names) + " on " + app.Name },
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.PatchAppEnv(ctx, app.ID, values, nil)
			return err
		},
		out: func(o *configureJSON) { o.Set = names },
	})
}

func runEnvUnset(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, args []string) error {
	if len(args) == 0 {
		return usageLine("kx env unset", "kx env unset needs at least one name.", "OLD_FLAG")
	}
	seen := map[string]bool{}
	for i, k := range args {
		if strings.Contains(k, "=") {
			return usageLine("kx env unset", fmt.Sprintf("Argument %d holds a =: kx env unset takes names only.", i+1), "OLD_FLAG")
		}
		if !envNameRe.MatchString(k) {
			return usageLine("kx env unset", badEnvName(k), "OLD_FLAG")
		}
		if seen[k] {
			return &kxerr.Usage{What: k + " is given twice.", Fix: "Give each variable once."}
		}
		seen[k] = true
	}
	names := append([]string(nil), args...)
	sort.Strings(names)
	return runConfigure(ctx, a, cmd, f, appChange{
		command: "kx env",
		step:    func(app *api.App) (string, string) { return "Unset", listOf(names) + " on " + app.Name },
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.PatchAppEnv(ctx, app.ID, nil, args)
			return err
		},
		out: func(o *configureJSON) { o.Unset = names },
	})
}

// envNameRe is what a container's environment allows as a variable name.
var envNameRe = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]{0,254}$`)

func badEnvName(k string) string {
	return fmt.Sprintf("%q isn't an environment variable name: use letters, digits, _ . and -, not starting with a digit.", k)
}

// envValues reads the variables kx env set was given: KEY=VALUE arguments,
// KEY arguments (the value of kx's own KEY variable), and the KEY=VALUE
// lines of fromFile. Errors name a variable or a position, never a value.
func envValues(a *App, fromFile string, args []string) (map[string]string, error) {
	const example = "LOG_LEVEL=debug"
	values := map[string]string{}
	add := func(k, v string) error {
		if _, dup := values[k]; dup {
			return &kxerr.Usage{What: k + " is given twice.", Fix: "Give each variable once."}
		}
		values[k] = v
		return nil
	}
	if fromFile != "" {
		lines, err := readEnvFile(a, fromFile)
		if err != nil {
			return nil, err
		}
		for _, kv := range lines {
			if err := add(kv[0], kv[1]); err != nil {
				return nil, err
			}
		}
	}
	for i, arg := range args {
		k, v, hasValue := strings.Cut(arg, "=")
		switch {
		case hasValue && k == "":
			return nil, usageLine("kx env set", fmt.Sprintf("Argument %d has no name before the =.", i+1), example)
		case hasValue && !envNameRe.MatchString(k):
			return nil, usageLine("kx env set", badEnvName(k), example)
		case !hasValue:
			// A bare argument could be a value typed by mistake: never
			// repeat it.
			if v = a.Getenv(arg); !envNameRe.MatchString(arg) || v == "" {
				return nil, usageLine("kx env set", fmt.Sprintf("Argument %d has no = and kx's environment has no variable by that name, so it has no value to set.", i+1), example)
			}
			k = arg
		}
		if err := add(k, v); err != nil {
			return nil, err
		}
	}
	if len(values) == 0 {
		return nil, usageLine("kx env set", "kx env set needs at least one KEY=VALUE.", example)
	}
	return values, nil
}

const maxEnvFile = 1 << 20

// readEnvFile reads KEY=VALUE lines: blank lines and # comments are
// skipped, "export " in front is allowed, and a value may be in double
// quotes (with \n, \" and \\) or single quotes (as written). Spaces around
// an unquoted value are dropped. Errors name the line, never its text.
func readEnvFile(a *App, name string) ([][2]string, error) {
	path := name
	if !filepath.IsAbs(path) && a.Dir != "" {
		path = filepath.Join(a.Dir, path)
	}
	cantRead := &kxerr.Usage{What: "kx can't read " + name + ".", Fix: "Check the --from-file path, then run the command again."}
	f, err := os.Open(path) //nolint:gosec // G304: the file the person named
	if err != nil {
		return nil, cantRead
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFile+1))
	if err != nil || len(data) > maxEnvFile {
		return nil, cantRead
	}
	var out [][2]string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64<<10), maxEnvFile)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bad := &kxerr.Usage{What: fmt.Sprintf("Line %d of %s isn't KEY=VALUE.", n, name), Fix: "Fix that line, then run the command again."}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envNameRe.MatchString(k) {
			return nil, bad
		}
		v, ok = envFileValue(strings.TrimSpace(v))
		if !ok {
			return nil, bad
		}
		out = append(out, [2]string{k, v})
	}
	if err := sc.Err(); err != nil {
		return nil, cantRead
	}
	return out, nil
}

// envFileValue unquotes a value from an env file.
func envFileValue(v string) (string, bool) {
	switch {
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1], true
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		var b strings.Builder
		in := v[1 : len(v)-1]
		for i := 0; i < len(in); i++ {
			if in[i] != '\\' || i+1 == len(in) {
				if in[i] == '"' {
					return "", false
				}
				b.WriteByte(in[i])
				continue
			}
			i++
			switch in[i] {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\':
				b.WriteByte(in[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(in[i])
			}
		}
		return b.String(), true
	case strings.HasPrefix(v, "'") || strings.HasPrefix(v, `"`):
		return "", false // an opening quote with no closing one (or a value over several lines)
	}
	return v, true
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
