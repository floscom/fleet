// Package sandbox runs agents in Docker containers.
//
// fleet stays terminal-first: the tmux pane of a sandboxed agent runs
// `docker run -it --rm ...` instead of the agent CLI, so attaching, sending
// text, exit capture and re-adoption after a daemon restart work unchanged.
// The container is named after the agent and labelled with the daemon's
// home, so the daemon can remove it when the agent ends (killing the docker
// client does not stop a container) and reap orphans at startup.
//
// Paths are mounted at the same location inside the container as on the
// host. Worktrees, git metadata, generated settings files and hook commands
// therefore need no path translation.
package sandbox

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Dockerfile builds the default agent image (see `fleet sandbox build`).
//
//go:embed Dockerfile
var Dockerfile []byte

// Labels set on every container fleet creates.
const (
	LabelAgent = "fleet.agent"
	LabelHome  = "fleet.home"
)

// DetachKeys replaces Docker's ctrl-p,ctrl-q detach sequence, which would
// swallow Ctrl-P in agent TUIs. It only has to be something nobody types.
const DetachKeys = "ctrl-^,ctrl-@,ctrl-^"

// Docker runs the docker CLI.
type Docker struct {
	// Binary is the docker executable; "" means "docker" from PATH.
	Binary string
}

// Mount is a bind mount.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// RunSpec describes one agent container.
type RunSpec struct {
	Name   string
	Image  string
	Labels map[string]string
	// User is "uid:gid".
	User    string
	Workdir string
	Mounts  []Mount
	// EnvFile holds the container environment (see WriteEnvFile).
	EnvFile string
	Network string
	// ExtraArgs are inserted before the image (user configured docker flags).
	ExtraArgs []string
	// Argv is the command run in the container.
	Argv []string
}

// RunArgv returns the full `docker run` command line for s. The container
// is interactive with a TTY (it runs inside a tmux pane), removed when it
// exits, runs tini as pid 1 and has no capabilities. no-new-privileges is
// not set by default: under snap-packaged Docker's AppArmor profile it makes
// every exec fail. Add it with ExtraArgs where it works.
func (d *Docker) RunArgv(s RunSpec) []string {
	argv := []string{d.bin(), "run", "--rm", "-it", "--init",
		"--name", s.Name,
		"--detach-keys", DetachKeys,
		"--pull", "never",
		"--cap-drop", "ALL",
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--label", k+"="+s.Labels[k])
	}
	if s.User != "" {
		argv = append(argv, "--user", s.User)
	}
	if s.Workdir != "" {
		argv = append(argv, "--workdir", s.Workdir)
	}
	if s.EnvFile != "" {
		argv = append(argv, "--env-file", s.EnvFile)
	}
	if s.Network != "" {
		argv = append(argv, "--network", s.Network)
	}
	for _, m := range s.Mounts {
		argv = append(argv, "--mount", m.flag())
	}
	argv = append(argv, s.ExtraArgs...)
	argv = append(argv, s.Image)
	return append(argv, s.Argv...)
}

// flag renders m for --mount. Commas separate --mount fields, and the value
// is parsed as CSV, so a path containing a comma or quote is quoted.
func (m Mount) flag() string {
	f := "type=bind," + csvField("source="+m.Source) + "," + csvField("target="+m.Target)
	if m.ReadOnly {
		f += ",readonly"
	}
	return f
}

func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// WriteEnvFile writes env as a docker --env-file (mode 0600), so values stay
// out of process listings. Docker env files cannot hold newlines; such
// variables are left out and their names returned.
func WriteEnvFile(path string, env map[string]string) (skipped []string, err error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		v := env[k]
		if k == "" || strings.ContainsAny(k, "=\n\r") || strings.ContainsAny(v, "\n\r") {
			skipped = append(skipped, k)
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return skipped, os.WriteFile(path, b.Bytes(), 0o600)
}

// Version returns the Docker server version, failing when the daemon is not
// reachable.
func (d *Docker) Version(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ErrNoImage is returned by CheckImage for an image that is not present.
var ErrNoImage = errors.New("image not found")

// CheckImage reports whether image exists locally.
func (d *Docker) CheckImage(ctx context.Context, image string) error {
	_, err := d.run(ctx, "image", "inspect", "--format", "{{.Id}}", "--", image)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such image") {
		return fmt.Errorf("%w: %s", ErrNoImage, image)
	}
	return err
}

// Probe checks that a container of image can bind-mount dir.
func (d *Docker) Probe(ctx context.Context, image, dir string) error {
	m := Mount{Source: dir, Target: "/fleet-probe", ReadOnly: true}
	_, err := d.run(ctx, "run", "--rm", "--pull", "never", "--network", "none",
		"--mount", m.flag(), "--entrypoint", "true", "--", image)
	return err
}

// Remove force-removes a container. A missing container is not an error.
func (d *Docker) Remove(ctx context.Context, name string) error {
	_, err := d.run(ctx, "rm", "--force", "--", name)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such container") || strings.Contains(msg, "already in progress") {
			return nil
		}
	}
	return err
}

// Containers returns the names of all containers (running or not) that
// carry label=value.
func (d *Docker) Containers(ctx context.Context, label, value string) ([]string, error) {
	out, err := d.run(ctx, "ps", "--all", "--no-trunc", "--filter", "label="+label+"="+value, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// Build builds dockerfile as tag, streaming docker's output to w.
func (d *Docker) Build(ctx context.Context, tag string, dockerfile []byte, pull bool, w io.Writer) error {
	args := []string{"build", "--tag", tag}
	if pull {
		args = append(args, "--pull", "--no-cache")
	}
	cmd := exec.CommandContext(ctx, d.bin(), append(args, "-")...)
	cmd.Stdin = bytes.NewReader(dockerfile)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

func (d *Docker) bin() string {
	if d.Binary != "" {
		return d.Binary
	}
	return "docker"
}

// run executes docker and returns stdout; errors carry stderr.
func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n\nRun 'docker")
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
