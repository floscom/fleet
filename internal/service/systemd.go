package service

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// systemd manages a systemd unit: a user unit, or a system one for root.
type systemd struct {
	name   string // unit name without ".service"
	dir    string
	system bool
}

func (s *systemd) unit() string { return s.name + ".service" }

func (s *systemd) Path() string { return filepath.Join(s.dir, s.unit()) }

func (s *systemd) Kind() string {
	if s.system {
		return "systemd system unit"
	}
	return "systemd user unit"
}

// ctl runs systemctl in the unit's scope.
func (s *systemd) ctl(args ...string) (string, error) {
	if !s.system {
		args = append([]string{"--user"}, args...)
	}
	out, err := run("systemctl", args...)
	if err != nil && !s.system && strings.Contains(out, "bus") {
		// "Failed to connect to bus": no user manager for this login,
		// e.g. after su or sudo -u without a session.
		return out, fmt.Errorf("%w\nsystemctl --user needs a login session of this user (log in over ssh or the console, not su)", err)
	}
	return out, err
}

func (s *systemd) Install(spec Spec) ([]string, error) {
	if err := writeFile(s.Path(), s.render(spec)); err != nil {
		return nil, err
	}
	if _, err := s.ctl("daemon-reload"); err != nil {
		return nil, err
	}
	if _, err := s.ctl("enable", s.unit()); err != nil {
		return nil, err
	}
	if _, err := s.ctl("restart", s.unit()); err != nil {
		return nil, err
	}
	if s.system {
		return nil, nil
	}
	return s.ensureLinger(), nil
}

// ensureLinger turns on lingering for the user: without it, systemd starts
// user units only at login and stops them, with every agent, when the last
// session of the user ends.
func (s *systemd) ensureLinger() []string {
	u, err := user.Current()
	if err != nil {
		return []string{"could not check lingering: " + err.Error()}
	}
	if out, err := run("loginctl", "show-user", u.Username, "--property=Linger", "--value"); err == nil && out == "yes" {
		return nil
	}
	if _, err := run("loginctl", "enable-linger", u.Username); err != nil {
		return []string{
			"could not turn on lingering, so the service only runs while " + u.Username + " is logged in: " + err.Error(),
			"turn it on with: sudo loginctl enable-linger " + u.Username,
		}
	}
	return []string{"turned on lingering for " + u.Username + ": the service starts at boot and keeps running after logout"}
}

func (s *systemd) Uninstall() error {
	if !exists(s.Path()) {
		return nil
	}
	if _, err := s.ctl("disable", "--now", s.unit()); err != nil {
		return err
	}
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _ = s.ctl("reset-failed", s.unit())
	_, err := s.ctl("daemon-reload")
	return err
}

func (s *systemd) Start() error {
	_, err := s.ctl("start", s.unit())
	return err
}

func (s *systemd) Stop() error {
	_, err := s.ctl("stop", s.unit())
	return err
}

func (s *systemd) Status() (Status, error) {
	st := Status{Installed: exists(s.Path())}
	if !st.Installed {
		return st, nil
	}
	out, err := s.ctl("show", s.unit(), "--property=ActiveState,SubState,UnitFileState,MainPID")
	if err != nil {
		return st, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	st.Enabled = props["UnitFileState"] == "enabled"
	st.Running = props["ActiveState"] == "active"
	st.Pid, _ = strconv.Atoi(props["MainPID"])
	st.Detail = props["ActiveState"]
	if sub := props["SubState"]; sub != "" && sub != st.Detail {
		st.Detail += " (" + sub + ")"
	}
	return st, nil
}

// render returns the unit file for spec.
func (s *systemd) render(spec Spec) []byte {
	var b strings.Builder
	ctl := "systemctl --user"
	if s.system {
		ctl = "systemctl"
	}
	fmt.Fprintf(&b, "# Written by `fleet service install`, which rewrites it: put local\n")
	fmt.Fprintf(&b, "# changes in a drop-in (%s edit %s).\n", ctl, s.name)
	b.WriteString("[Unit]\nDescription=fleet daemon (coding agents in tmux)\nDocumentation=https://github.com/floscom/fleet\n")
	if s.system {
		b.WriteString("After=network-online.target\nWants=network-online.target\n")
	}
	b.WriteString("\n[Service]\nType=simple\n")
	words := []string{spec.Binary, "daemon"}
	words = append(words, spec.Args...)
	for i, w := range words {
		words[i] = execQuote(w)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(words, " "))
	for _, k := range sortedKeys(spec.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", quote(k+"="+spec.Env[k]))
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n")
	b.WriteString("# Agents run in the tmux server the daemon started: stopping the\n# daemon must not kill it.\nKillMode=process\n")
	if spec.Log != "" {
		log := strings.ReplaceAll(spec.Log, "%", "%%")
		fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=append:%s\n", log, log)
	}
	target := "default.target"
	if s.system {
		target = "multi-user.target"
	}
	fmt.Fprintf(&b, "\n[Install]\nWantedBy=%s\n", target)
	return []byte(b.String())
}

// quote double-quotes s for a unit file, escaping specifiers.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

// execQuote is quote for ExecStart, where "$" also expands variables.
func execQuote(s string) string {
	return strings.ReplaceAll(quote(s), "$", "$$")
}
