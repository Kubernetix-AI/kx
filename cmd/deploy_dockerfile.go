package cmd

import (
	"context"
	"errors"
	"path/filepath"
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

	exposed, err := scaffold.EnvExposed(dir)
	if err != nil {
		return err
	}
	if len(exposed) > 0 {
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
			return err
		}
	}

	wrote, err := scaffold.Write(dir, *app)
	if err != nil {
		return err
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
	for _, name := range wrote {
		if name == ".dockerignore" {
			p.Info("Wrote .dockerignore, which keeps .env files and .git out of the image.")
		}
	}
	p.Info("")
	return nil
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
