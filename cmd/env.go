package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/ci"
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
	var in envInput
	set := &cobra.Command{
		Use:   "set NAME=VALUE...",
		Short: "Set environment variables, then deploy the app",
		Long: `Set (add or change) environment variables of the app, keep the others, then deploy the app
as configured and wait until it's live, like kx deploy. kx never prints a value.

For a secret, use --stdin: kx asks for the value without showing it (or reads it from
standard input), so it stays out of your shell history. A NAME with no =VALUE takes its
value from kx's own environment, so a CI secret never appears on the command line either.
--from-file reads NAME=VALUE lines from a file such as .env (no ${...} expansion).
Names use letters, digits and _; an empty value needs --allow-empty.`,
		Example: `  kx env set LOG_LEVEL=debug FEATURE_X=on
  kx env set STRIPE_KEY --stdin             asks for the value, or reads it from standard input
  kx env set STRIPE_KEY                     the value of $STRIPE_KEY
  kx env set --from-file .env.production
  kx env set LOG_LEVEL=info --no-deploy     save it; the next deploy applies it`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return asChange(runEnvSet(cmd.Context(), a, cmd, setFlags, in, args))
		},
	}
	addConfigureFlags(set, &setFlags)
	set.Flags().StringVar(&in.fromFile, "from-file", "", "read NAME=VALUE lines from this `file` (such as .env)")
	set.Flags().BoolVar(&in.stdin, "stdin", false, "read the one variable's value from standard input (or ask for it), never shown")
	set.Flags().BoolVar(&in.allowEmpty, "allow-empty", false, "allow empty values (to remove a variable, use kx env unset)")

	var unsetFlags configureFlags
	unset := &cobra.Command{
		Use:     "unset KEY...",
		Short:   "Remove environment variables, then deploy the app",
		Example: "  kx env unset OLD_FLAG\n  kx env unset OLD_FLAG LEGACY_URL --no-deploy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return asChange(runEnvUnset(cmd.Context(), a, cmd, unsetFlags, args))
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

// envInput is where kx env set's values come from besides its arguments.
type envInput struct {
	fromFile   string
	stdin      bool
	allowEmpty bool
}

func runEnvSet(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, in envInput, args []string) error {
	values, err := envValues(ctx, a, in, args)
	if err != nil {
		return err
	}
	maskValues(a, values)
	names := sortedKeys(values)
	return runConfigure(ctx, a, cmd, f, appChange{
		command: "kx env",
		step:    func(app *api.App) (string, string) { return "Set", listOf(names) + " on " + app.Name },
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.PatchAppEnv(ctx, app.ID, values, nil)
			return envRefused(app, values, err)
		},
		undone: func(before *api.App) func(*api.App) (bool, bool) {
			// Only a name the app didn't have shows the change is gone:
			// kx never sees values.
			var added []string
			for _, k := range names {
				if _, had := before.EnvVars[k]; !had {
					added = append(added, k)
				}
			}
			return func(fresh *api.App) (bool, bool) {
				if len(added) == 0 {
					return false, false
				}
				_, still := fresh.EnvVars[added[0]]
				return !still, true
			}
		},
		unknowable: "kx sees the names of variables, not their values, so it can't check that Kubernetix put %[1]s back to %[2]s.",
		checkWith:  envCheck,
		out:        func(o *configureJSON) { o.Set = names },
	})
}

func runEnvUnset(ctx context.Context, a *App, cmd *cobra.Command, f configureFlags, args []string) error {
	if len(args) == 0 {
		return usageLine("kx env unset", "kx env unset needs at least one name.", "OLD_FLAG")
	}
	seen := map[string]bool{}
	for i, k := range args {
		if !envNameRe.MatchString(k) {
			// It could be a value typed by mistake: name the position only.
			return usageLine("kx env unset", fmt.Sprintf("Argument %d isn't a name: kx env unset takes names only.", i+1), "OLD_FLAG")
		}
		if seen[k] {
			return &kxerr.Usage{What: k + " is given twice.", Fix: "Give each variable once."}
		}
		seen[k] = true
	}
	names := append([]string(nil), args...)
	sort.Strings(names)
	// Only the names the app has are removed; the others are counted, not
	// named (a name typed here could be a value by mistake).
	var present []string
	missing := 0
	return runConfigure(ctx, a, cmd, f, appChange{
		command: "kx env",
		prepare: func(_ context.Context, _ *api.Client, app *api.App) (string, error) {
			for _, k := range args {
				if _, ok := app.EnvVars[k]; ok {
					present = append(present, k)
				}
			}
			missing = len(args) - len(present)
			switch {
			case len(present) > 0:
				return "", nil
			case len(args) == 1:
				return "Nothing to change: " + app.Name + " has no variable by that name.", nil
			}
			return "Nothing to change: " + app.Name + " has none of those variables.", nil
		},
		step: func(app *api.App) (string, string) {
			return "Unset", listOf(sortedCopy(present)) + " on " + app.Name
		},
		note: func(app *api.App) string {
			switch missing {
			case 0:
				return ""
			case 1:
				return "1 of the names isn't set on " + app.Name + "."
			}
			return fmt.Sprintf("%d of the names aren't set on %s.", missing, app.Name)
		},
		save: func(ctx context.Context, c *api.Client, app *api.App) error {
			_, err := c.PatchAppEnv(ctx, app.ID, nil, present)
			return err
		},
		undone: func(*api.App) func(*api.App) (bool, bool) {
			return func(fresh *api.App) (bool, bool) {
				_, back := fresh.EnvVars[present[0]]
				return back, true
			}
		},
		checkWith: envCheck,
		out:       func(o *configureJSON) { o.Unset = sortedCopy(present) },
	})
}

// envNameRe is the names kx env takes: letters, digits and _, not
// starting with a digit, at most 128 characters.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

const envExample = "LOG_LEVEL=debug"

// Errors about kx env set's arguments name the argument's position and
// never its text: an argument could be a secret typed without NAME=.
func notNameValue(i int) error {
	return usageLine("kx env set", fmt.Sprintf("Argument %d isn't NAME=VALUE: names use letters, digits and _, and don't start with a digit.", i), envExample)
}

// emptyValue refuses an empty value given without --allow-empty. where is
// "Argument 2" or "Line 5 of .env".
func emptyValue(where string, arg bool) error {
	if arg {
		return &kxerr.Usage{
			What: where + " sets an empty value: if it's a value, write NAME=value; to set an empty value, add --allow-empty.",
			Fix:  "To remove a variable instead: kx env unset NAME",
		}
	}
	return &kxerr.Usage{What: where + " sets an empty value; to set it empty, add --allow-empty.", Fix: "To remove a variable instead: kx env unset NAME"}
}

// maxEnvValue is the longest value Kubernetix takes, in bytes.
const maxEnvValue = 32768

// tooLarge refuses a value over maxEnvValue. where is "Argument 2", "Line
// 5 of .env" or "Standard input"; the value itself is never shown.
func tooLarge(where, verb string, n int) error {
	return &kxerr.Usage{
		What: fmt.Sprintf("%s %s a %d-byte value; Kubernetix takes at most %d.", where, verb, n, maxEnvValue),
		Fix:  "Store a value that large as a secret in the dashboard (Secrets) and bind it to the app.",
	}
}

// envValues reads the variables kx env set was given: NAME=VALUE
// arguments, NAME arguments (the value of kx's own NAME variable), the
// NAME=VALUE lines of --from-file, or one NAME with --stdin. Errors name a
// variable, a position or a line, never a value.
func envValues(ctx context.Context, a *App, in envInput, args []string) (map[string]string, error) {
	values := map[string]string{}
	add := func(k, v string) error {
		if _, dup := values[k]; dup {
			return &kxerr.Usage{What: k + " is given twice.", Fix: "Give each variable once."}
		}
		values[k] = v
		return nil
	}
	if in.stdin {
		return stdinValue(ctx, a, in, args)
	}
	if in.fromFile != "" {
		lines, err := readEnvFile(a, in.fromFile, in.allowEmpty)
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
		case !envNameRe.MatchString(k) && hasValue:
			return nil, notNameValue(i + 1)
		case !hasValue:
			if v = a.Getenv(arg); !envNameRe.MatchString(arg) || v == "" {
				return nil, usageLine("kx env set", fmt.Sprintf("Argument %d isn't NAME=VALUE, nor the name of a variable in kx's environment.", i+1), envExample)
			}
		case strings.Trim(v, "=") == "" && v != "":
			// The = padding of a base64 secret typed without a name.
			return nil, usageLine("kx env set", fmt.Sprintf("Argument %d looks like a value: write NAME=value, or NAME to take the value from kx's environment.", i+1), envExample)
		case v == "" && !in.allowEmpty:
			return nil, emptyValue(fmt.Sprintf("Argument %d", i+1), true)
		case len(v) > maxEnvValue:
			return nil, tooLarge(fmt.Sprintf("Argument %d", i+1), "has", len(v))
		}
		if err := add(k, v); err != nil {
			return nil, err
		}
	}
	if len(values) == 0 {
		return nil, usageLine("kx env set", "kx env set needs at least one NAME=VALUE.", envExample)
	}
	return values, nil
}

// maxStdinValue is the longest value --stdin reads.
const maxStdinValue = 1 << 20

// stdinValue reads the one value --stdin is for: asked for without echo on
// a terminal, else all of standard input without its last line break.
func stdinValue(ctx context.Context, a *App, in envInput, args []string) (map[string]string, error) {
	if len(args) != 1 || !envNameRe.MatchString(args[0]) || in.fromFile != "" {
		return nil, &kxerr.Usage{What: "--stdin reads one value: give just its name, like kx env set API_KEY --stdin.", Fix: "Run: kx env set API_KEY --stdin"}
	}
	name, p := args[0], a.printer()
	var v string
	var err error
	if p.InTTY && p.ReadPassword != nil {
		v, err = p.ReadSecret(ctx, "Value for "+name+": ")
	} else {
		v, err = p.ReadAll(ctx, maxStdinValue)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &kxerr.Usage{What: "kx couldn't read the value from standard input.", Fix: "Run: echo \"$VALUE\" | kx env set " + name + " --stdin"}
	}
	if v == "" && !in.allowEmpty {
		return nil, &kxerr.Usage{What: "Standard input holds an empty value; to set an empty value, add --allow-empty.", Fix: "To remove a variable instead: kx env unset " + name}
	}
	if len(v) > maxEnvValue {
		return nil, tooLarge("Standard input", "holds", len(v))
	}
	return map[string]string{name: v}, nil
}

// maskValues tells GitHub Actions to hide the values kx sets from the
// job's log (the app's own log lines, which kx shows when a deploy fails,
// could print them), before kx prints anything else.
func maskValues(a *App, values map[string]string) {
	if ci.Detect(a.Getenv).Provider != ci.GitHub {
		return
	}
	p := a.printer()
	for _, k := range sortedKeys(values) {
		p.AddMask(values[k])
	}
}

const maxEnvFile = 1 << 20

// readEnvFile reads NAME=VALUE lines the way the common dotenv tools do: a
// UTF-8 BOM at the start is dropped, blank lines and # comments are
// skipped, "export" and a space in front are allowed, an unquoted value
// ends at " #" (a comment) and loses its outer spaces, and a value in
// double quotes takes \n, \" and \\ while one in single quotes is taken as
// written. ${...} is not expanded. Errors name the line, never its text.
func readEnvFile(a *App, name string, allowEmpty bool) ([][2]string, error) {
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
	text := strings.TrimPrefix(string(data), "\ufeff")
	var out [][2]string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), maxEnvFile)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bad := &kxerr.Usage{What: fmt.Sprintf("Line %d of %s isn't NAME=VALUE.", n, name), Fix: "Fix that line, then run the command again."}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envNameRe.MatchString(k) {
			return nil, bad
		}
		v, ok = envFileValue(strings.TrimSpace(v))
		if !ok {
			return nil, bad
		}
		if v == "" && !allowEmpty {
			return nil, emptyValue(fmt.Sprintf("Line %d of %s", n, name), false)
		}
		if len(v) > maxEnvValue {
			return nil, tooLarge(fmt.Sprintf("Line %d of %s", n, name), "has", len(v))
		}
		out = append(out, [2]string{k, v})
	}
	if err := sc.Err(); err != nil {
		return nil, cantRead
	}
	return out, nil
}

// envFileValue unquotes a value from an env file; ok is false for a
// quote that doesn't close on the line.
func envFileValue(v string) (string, bool) {
	switch {
	case strings.HasPrefix(v, "'"):
		end := strings.Index(v[1:], "'")
		if end < 0 || strings.TrimSpace(v[end+2:]) != "" && !strings.HasPrefix(strings.TrimSpace(v[end+2:]), "#") {
			return "", false
		}
		return v[1 : end+1], true
	case strings.HasPrefix(v, `"`):
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			switch c := v[i]; {
			case c == '\\' && i+1 < len(v):
				i++
				switch v[i] {
				case 'n':
					b.WriteByte('\n')
				case '"', '\\':
					b.WriteByte(v[i])
				default:
					b.WriteByte('\\')
					b.WriteByte(v[i])
				}
			case c == '"':
				rest := strings.TrimSpace(v[i+1:])
				if rest != "" && !strings.HasPrefix(rest, "#") {
					return "", false
				}
				return b.String(), true
			default:
				b.WriteByte(c)
			}
		}
		return "", false // no closing quote (or the closing one is escaped)
	}
	// An unquoted value ends where a comment starts.
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i]), true
		}
	}
	return v, true
}

// envRefused words Kubernetix's refusal of the variables an app would hold
// after kx env set (invalid_env_var). kx checked what it sends, so the
// variable named is one the app already holds, or the total is over the
// limit. The platform's message names variables, never values.
func envRefused(app *api.App, set map[string]string, err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_env_var" {
		return err
	}
	what := "Kubernetix didn't accept the change: " + apiErr.Message
	name, named := strings.CutPrefix(apiErr.Field, "env_vars.")
	if _, ours := set[name]; named && name != "" && !ours {
		return &kxerr.Usage{Code: "invalid_env_var", What: what,
			Fix: fmt.Sprintf("%s is already on %s: remove it first (kx env unset %s), then run the command again.", name, app.Name, name)}
	}
	return &kxerr.Usage{Code: "invalid_env_var", What: what,
		Fix: fmt.Sprintf("Remove variables %s no longer needs (kx env unset NAME), then run the command again.", app.Name)}
}

// envCheck is the command that shows an app's variables.
func envCheck(*api.App) string { return "kx env list" }

// sortedCopy returns names sorted, leaving names as it was.
func sortedCopy(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
