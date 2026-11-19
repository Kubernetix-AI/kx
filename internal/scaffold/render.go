package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"join": func(items []string) string { return strings.Join(items, " ") },
	"exec": execForm,
}).ParseFS(templateFiles, "templates/*.Dockerfile.tmpl"))

// Render returns the Dockerfile kx writes for d, and the .dockerignore it
// writes beside it when the folder has none.
func Render(d Detection) (dockerfile, dockerignore []byte, err error) {
	if d.Problem != "" {
		return nil, nil, errors.New("kx can't write a Dockerfile for this app: " + d.Problem)
	}
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, string(d.Stack)+".Dockerfile.tmpl", d); err != nil {
		return nil, nil, fmt.Errorf("no Dockerfile for %q: %w", d.Stack, err)
	}
	ignore, err := templateFiles.ReadFile("templates/dockerignore.tmpl")
	if err != nil {
		return nil, nil, err
	}
	return b.Bytes(), ignore, nil
}

// execForm writes a command as a JSON array (Docker's exec form), so a
// value from the folder's files can never become a second instruction.
func execForm(argv []string) (string, error) {
	for _, a := range argv {
		if strings.ContainsAny(a, "\r\n") {
			return "", fmt.Errorf("%q spans more than one line", a)
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(argv); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// Write creates dir's Dockerfile for d and, when dir has none, its
// .dockerignore, and returns the names it wrote. It never replaces a
// Dockerfile (or dockerfile) and never follows a link: each file is
// created only if nothing by that name exists. The .dockerignore comes
// first, so a Dockerfile kx wrote never exists without it.
func Write(dir string, d Detection) (wrote []string, err error) {
	dockerfile, dockerignore, err := Render(d)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Dockerfile", "dockerfile"} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			return nil, &fs.PathError{Op: "write", Path: path, Err: fs.ErrExist}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	ignored := false
	switch err := createNew(filepath.Join(dir, ".dockerignore"), dockerignore); {
	case err == nil:
		ignored = true
	case !errors.Is(err, fs.ErrExist):
		return nil, err
	}
	if err := createNew(filepath.Join(dir, "Dockerfile"), dockerfile); err != nil {
		if ignored {
			return []string{".dockerignore"}, err
		}
		return nil, err
	}
	wrote = []string{"Dockerfile"}
	if ignored {
		wrote = append(wrote, ".dockerignore")
	}
	return wrote, nil
}

// createNew writes data to a file that must not exist yet (O_EXCL also
// refuses a symbolic link, dangling or not).
func createNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302,G304: a Dockerfile is an ordinary project file, in the folder kx deploys
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
