package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchd manages a launchd job: a LaunchAgent of the user, or a
// LaunchDaemon for root.
type launchd struct {
	label  string
	dir    string
	domain string // gui/<uid> or system
	system bool
}

func (l *launchd) Path() string { return filepath.Join(l.dir, l.label+".plist") }

func (l *launchd) Kind() string {
	if l.system {
		return "launchd daemon"
	}
	return "launchd agent"
}

// domains are where the job may be loaded: a user's agent goes into the
// GUI domain, or into the user domain when nobody is logged in at the
// screen (only ssh), which has no GUI domain.
func (l *launchd) domains() []string {
	if l.system {
		return []string{l.domain}
	}
	return []string{l.domain, "user/" + strconv.Itoa(os.Getuid())}
}

// loaded returns the domain the job is loaded in, "" if none.
func (l *launchd) loaded() (string, string) {
	for _, d := range l.domains() {
		if out, err := run("launchctl", "print", d+"/"+l.label); err == nil {
			return d, out
		}
	}
	return "", ""
}

// bootstrap loads (and with RunAtLoad starts) the job in the first domain
// that takes it.
func (l *launchd) bootstrap() (string, error) {
	var errs []error
	for _, d := range l.domains() {
		// A job disabled earlier (launchctl disable) would not load.
		_, _ = run("launchctl", "enable", d+"/"+l.label)
		if _, err := run("launchctl", "bootstrap", d, l.Path()); err != nil {
			errs = append(errs, err)
			continue
		}
		return d, nil
	}
	return "", errors.Join(errs...)
}

func (l *launchd) bootout() error {
	d, _ := l.loaded()
	if d == "" {
		return nil
	}
	_, err := run("launchctl", "bootout", d+"/"+l.label)
	return err
}

func (l *launchd) Install(spec Spec) ([]string, error) {
	if err := writeFile(l.Path(), l.render(spec)); err != nil {
		return nil, err
	}
	if err := l.bootout(); err != nil {
		return nil, err
	}
	d, err := l.bootstrap()
	if err != nil {
		return nil, err
	}
	if !l.system && d != l.domain {
		return []string{"nobody is logged in at the screen, so the agent was loaded for this ssh login; it starts on its own only when " +
			"you log in at the screen (turn on automatic login for a Mac without one)"}, nil
	}
	return nil, nil
}

func (l *launchd) Uninstall() error {
	if err := l.bootout(); err != nil {
		return err
	}
	if err := os.Remove(l.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *launchd) Start() error {
	if d, _ := l.loaded(); d != "" {
		_, err := run("launchctl", "kickstart", d+"/"+l.label)
		return err
	}
	_, err := l.bootstrap()
	return err
}

// Stop unloads the job: a loaded one would be started again by KeepAlive
// if the daemon does not exit cleanly. It loads again at the next login
// or boot.
func (l *launchd) Stop() error { return l.bootout() }

func (l *launchd) Status() (Status, error) {
	st := Status{Installed: exists(l.Path())}
	if !st.Installed {
		return st, nil
	}
	st.Enabled = true
	if out, err := run("launchctl", "print-disabled", l.domain); err == nil {
		for _, line := range strings.Split(out, "\n") {
			k, v, ok := strings.Cut(line, "=>")
			if ok && strings.Trim(strings.TrimSpace(k), `"`) == l.label {
				v = strings.TrimSpace(v)
				st.Enabled = v != "true" && v != "disabled"
			}
		}
	}
	d, out := l.loaded()
	if d == "" {
		st.Detail = "not loaded"
		return st, nil
	}
	st.Detail = "loaded"
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		switch {
		case !ok:
		case k == "state" && st.Detail == "loaded":
			st.Detail = v
			st.Running = v == "running"
		case k == "pid" && st.Pid == 0:
			st.Pid, _ = strconv.Atoi(v)
		}
	}
	return st, nil
}

// render returns the plist for spec.
func (l *launchd) render(spec Spec) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	str := func(s string) string { return "<string>" + esc(s) + "</string>" }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by "fleet service install", which rewrites it. -->
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t%s\n", str(l.label))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{spec.Binary, "daemon"}, spec.Args...) {
		fmt.Fprintf(&b, "\t\t%s\n", str(a))
	}
	b.WriteString("\t</array>\n")
	if len(spec.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(spec.Env) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t%s\n", esc(k), str(spec.Env[k]))
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	// Restart after a crash, not after a clean stop.
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	// Agents run in the tmux server the daemon started: launchd must not
	// kill it with the daemon.
	b.WriteString("\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	// No background throttling of CPU and I/O: agents build and test.
	b.WriteString("\t<key>ProcessType</key>\n\t<string>Interactive</string>\n")
	if spec.Log != "" {
		fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t%s\n\t<key>StandardErrorPath</key>\n\t%s\n", str(spec.Log), str(spec.Log))
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}
