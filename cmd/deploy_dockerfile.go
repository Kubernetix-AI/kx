package cmd

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kubernetix-ai/kx/internal/api"
	"github.com/kubernetix-ai/kx/internal/kxerr"
	"github.com/kubernetix-ai/kx/internal/scaffold"
	"github.com/kubernetix-ai/kx/internal/ui"
)

// writeDockerfile offers to write a Dockerfile for the app in dir, which
// has none, and writes it when the person says yes. It returns nil only
// once the Dockerfile is there. kx writes one only on a terminal and
// outside CI, where someone sees the file before it is built; and never
// one that would copy a .env file into the image.
func (d *deployRun) writeDockerfile(ctx context.Context, dir string) error {
	s, p := d.s, d.s.P
	// A Dockerfile that's there but can't be read is neither built nor
	// replaced, wherever kx runs.
	if path := scaffold.UnreadableDockerfile(dir); path != "" {
		return &kxerr.NoDockerfile{Dir: dir, What: "kx can't read " + path + ", and won't replace it.", Fix: "Fix or remove " + path + ", then run kx deploy again."}
	}
	switch {
	case s.CI.IsCI:
		return &kxerr.NoDockerfile{Dir: dir, Why: kxerr.NoDockerfileCI}
	case !p.InTTY || !p.TTY:
		return &kxerr.NoDockerfile{Dir: dir}
	}
	found, err := scaffold.Detect(dir)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return &kxerr.NoDockerfile{Dir: dir, Why: kxerr.NoDockerfileUndetected}
	}
	if len(found) == 1 && found[0].Problem != "" {
		return &kxerr.NoDockerfile{Dir: dir, What: found[0].Problem, Fix: found[0].Fix}
	}

	p.Info("%s", api.Explain(&kxerr.NoDockerfile{Dir: dir}).What)
	app, err := chooseStack(ctx, p, found)
	if err != nil {
		return err
	}
	if app == nil {
		return &kxerr.NoDockerfile{Dir: dir}
	}
	if app.Problem != "" {
		return &kxerr.NoDockerfile{Dir: dir, What: app.Problem, Fix: app.Fix}
	}
	if err := keepEnvOut(ctx, p, dir); err != nil {
		return err
	}

	wrote, err := scaffold.Write(dir, *app)
	if slices.Contains(wrote, ".dockerignore") && err != nil {
		p.Info("Wrote .dockerignore, which keeps .env files and .git out of the image.")
	}
	if err != nil {
		return writeFailed(dir, err)
	}
	dockerfile, _, err := scaffold.Render(*app)
	if err != nil {
		return err
	}
	p.Info("Wrote %s:", filepath.Join(dir, "Dockerfile"))
	for _, line := range strings.Split(strings.TrimSuffix(string(dockerfile), "\n"), "\n") {
		if line == "" {
			p.Info("")
			continue
		}
		p.Info("    %s", line)
	}
	if slices.Contains(wrote, ".dockerignore") {
		p.Info("Wrote .dockerignore, which keeps .env files and .git out of the image.")
	}
	p.Info("")
	return nil
}

// keepEnvOut makes sure no .env file in dir would be copied into the
// image: when the folder's .dockerignore lets one through, it asks to add
// the .env lines to it, and stops if the answer is no. A .dockerignore
// Docker can't read keeps nothing out, and kx can't add to it.
func keepEnvOut(ctx context.Context, p *ui.Printer, dir string) error {
	broken := scaffold.DockerignoreBroken(dir)
	exposed, err := scaffold.EnvExposed(dir)
	if err != nil {
		return err
	}
	switch {
	case broken && len(exposed) > 0:
		return &kxerr.NoDockerfile{Dir: dir,
			What: ".dockerignore is a broken link or not a file, so a .env file in this folder would be copied into the image.",
			Fix:  "Fix or remove .dockerignore, then run kx deploy again."}
	case broken:
		p.Warn(".dockerignore is a broken link or not a file, so it keeps nothing out of the image.")
		return nil
	case len(exposed) == 0:
		return nil
	}
	envExposed := &kxerr.NoDockerfile{Dir: dir, Why: kxerr.NoDockerfileEnvExposed}
	p.Warn("%s", api.Explain(envExposed).What)
	yes, err := p.ConfirmYes(ctx, "Add .env to .dockerignore?")
	if err != nil {
		return err
	}
	if !yes {
		return envExposed
	}
	if err := scaffold.IgnoreEnv(dir); err != nil {
		path := filepath.Join(dir, ".dockerignore")
		return &kxerr.NoDockerfile{Dir: dir,
			What: "kx couldn't add .env to " + path + ": " + fileErrReason(err) + ".",
			Fix:  "Add the lines **/.env and **/.env.* to .dockerignore yourself, then run kx deploy again."}
	}
	return nil
}

// writeFailed explains why scaffold.Write didn't write the Dockerfile.
func writeFailed(dir string, err error) error {
	path := filepath.Join(dir, "Dockerfile")
	var pe *fs.PathError
	if errors.As(err, &pe) {
		path = pe.Path
	}
	if errors.Is(err, fs.ErrExist) {
		return &kxerr.NoDockerfile{Dir: dir, What: path + " appeared while kx was asking, and kx doesn't replace it.", Fix: "Run kx deploy again to build with it."}
	}
	return &kxerr.NoDockerfile{Dir: dir, What: "kx couldn't write " + path + ": " + fileErrReason(err) + ".", Fix: "Check that you can write to " + dir + ", then run kx deploy again."}
}

// fileErrReason is the operating system's reason for a file error
// ("permission denied"), without the path it already names.
func fileErrReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// chooseStack asks whether to write a Dockerfile for the one app kx
// found, or which of several it is. It returns nil for no.
func chooseStack(ctx context.Context, p *ui.Printer, found []scaffold.Detection) (*scaffold.Detection, error) {
	if len(found) == 1 {
		yes, err := p.ConfirmYes(ctx, "This looks like "+found[0].Label+". Create one now?")
		if err != nil || !yes {
			return nil, err
		}
		return &found[0], nil
	}
	options := make([]string, 0, len(found)+1)
	for _, f := range found {
		options = append(options, f.Label)
	}
	options = append(options, "None of these")
	i, err := p.Choose(ctx, "This folder looks like more than one kind of app. Which one is it?", options, -1)
	if errors.Is(err, ui.ErrNoChoice) || (err == nil && i == len(found)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &found[i], nil
}
