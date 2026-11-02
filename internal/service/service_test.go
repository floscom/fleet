package service

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeRun replaces runCmd: it records every command and answers from
// replies, keyed by the command line ("" = success with no output).
func fakeRun(t *testing.T, replies map[string]string, fail map[string]bool) *[]string {
	t.Helper()
	var calls []string
	old := runCmd
	runCmd = func(name string, args ...string) ([]byte, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, line)
		if fail[line] {
			return []byte(replies[line]), errors.New("exit status 1")
		}
		return []byte(replies[line]), nil
	}
	t.Cleanup(func() { runCmd = old })
	return &calls
}

var spec = Spec{
	Binary: "/opt/my apps/fleet",
	Args:   []string{"--web", "off", "--listen", "100%$x"},
	Env:    map[string]string{"PATH": `/usr/bin:/a "b"`, "LANG": "C.UTF-8"},
	Log:    "/home/u/.fleet/daemon.log",
}

func TestSystemdRender(t *testing.T) {
	s := &systemd{name: "fleet", dir: t.TempDir()}
	got := string(s.render(spec))
	for _, want := range []string{
		`ExecStart="/opt/my apps/fleet" "daemon" "--web" "off" "--listen" "100%%$$x"`,
		`Environment="LANG=C.UTF-8"` + "\n" + `Environment="PATH=/usr/bin:/a \"b\""`,
		"KillMode=process\n",
		"Restart=on-failure\n",
		"StandardOutput=append:/home/u/.fleet/daemon.log\nStandardError=append:/home/u/.fleet/daemon.log\n",
		"WantedBy=default.target\n",
		"systemctl --user edit fleet",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "network-online") {
		t.Error("user unit orders after network-online.target, which user managers do not have")
	}
	sys := string((&systemd{name: "fleet", system: true}).render(spec))
	if !strings.Contains(sys, "WantedBy=multi-user.target") || !strings.Contains(sys, "After=network-online.target") {
		t.Errorf("system unit:\n%s", sys)
	}
}

func TestSystemdInstall(t *testing.T) {
	s := &systemd{name: "fleet", dir: filepath.Join(t.TempDir(), "user")}
	calls := fakeRun(t, map[string]string{}, nil)
	notes, err := s.Install(spec)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(s.Path()); err != nil || !strings.Contains(string(b), "ExecStart=") {
		t.Fatalf("unit not written: %v", err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable fleet.service",
		"systemctl --user restart fleet.service",
	}
	if len(*calls) < len(want) || strings.Join((*calls)[:3], "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q", *calls)
	}
	// Linger was off ("" from show-user), so it is turned on.
	last := (*calls)[len(*calls)-1]
	if !strings.HasPrefix(last, "loginctl enable-linger ") || len(notes) != 1 || !strings.Contains(notes[0], "turned on lingering") {
		t.Errorf("linger: calls %q, notes %q", *calls, notes)
	}
}

func TestSystemdLingerFails(t *testing.T) {
	s := &systemd{name: "fleet", dir: t.TempDir()}
	fakeRun(t, nil, nil)
	runCmd = func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" && args[0] == "enable-linger" {
			return []byte("Access denied"), errors.New("exit status 1")
		}
		return nil, nil
	}
	notes, err := s.Install(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || !strings.Contains(notes[1], "sudo loginctl enable-linger") {
		t.Errorf("notes = %q", notes)
	}
}

func TestSystemdStatus(t *testing.T) {
	s := &systemd{name: "fleet", dir: t.TempDir()}
	fakeRun(t, map[string]string{
		"systemctl --user show fleet.service --property=ActiveState,SubState,UnitFileState,MainPID": "MainPID=4242\nActiveState=active\nSubState=running\nUnitFileState=enabled\n",
	}, nil)
	if st, err := s.Status(); err != nil || st.Installed {
		t.Fatalf("no unit: %+v, %v", st, err)
	}
	if err := writeFile(s.Path(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status()
	want := Status{Installed: true, Enabled: true, Running: true, Pid: 4242, Detail: "active (running)"}
	if err != nil || st != want {
		t.Errorf("Status = %+v, %v; want %+v", st, err, want)
	}
}

func TestSystemdNoBus(t *testing.T) {
	s := &systemd{name: "fleet", dir: t.TempDir()}
	fakeRun(t, map[string]string{"systemctl --user start fleet.service": "Failed to connect to bus: No medium found"},
		map[string]bool{"systemctl --user start fleet.service": true})
	if err := s.Start(); err == nil || !strings.Contains(err.Error(), "login session") {
		t.Errorf("err = %v", err)
	}
}

func TestLaunchdRender(t *testing.T) {
	l := &launchd{label: "com.floscom.fleet", dir: t.TempDir(), domain: "gui/501"}
	got := l.render(Spec{Binary: "/usr/local/bin/fleet", Args: []string{"--web", "a&b"}, Env: spec.Env, Log: spec.Log})
	// Well-formed XML, with what launchd needs.
	d := xml.NewDecoder(strings.NewReader(string(got)))
	d.Strict = true
	for {
		if _, err := d.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("plist is not XML: %v\n%s", err, got)
			}
			break
		}
	}
	for _, want := range []string{
		"<string>/usr/local/bin/fleet</string>\n\t\t<string>daemon</string>\n\t\t<string>--web</string>\n\t\t<string>a&amp;b</string>",
		"<key>PATH</key>\n\t\t<string>/usr/bin:/a &#34;b&#34;</string>",
		"<key>AbandonProcessGroup</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>StandardErrorPath</key>\n\t<string>/home/u/.fleet/daemon.log</string>",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
}

func TestLaunchdInstallFallsBackToUserDomain(t *testing.T) {
	l := &launchd{label: "com.floscom.fleet", dir: t.TempDir(), domain: "gui/501"}
	uid := "user/" + strconv.Itoa(os.Getuid())
	calls := fakeRun(t, nil, map[string]bool{
		"launchctl print gui/501/com.floscom.fleet":                                      true,
		"launchctl print " + uid + "/com.floscom.fleet":                                  true,
		"launchctl bootstrap gui/501 " + filepath.Join(l.dir, "com.floscom.fleet.plist"): true,
	})
	notes, err := l.Install(spec)
	if err != nil {
		t.Fatal(err)
	}
	if last := (*calls)[len(*calls)-1]; last != "launchctl bootstrap "+uid+" "+l.Path() {
		t.Errorf("calls = %q", *calls)
	}
	if len(notes) != 1 {
		t.Errorf("notes = %q", notes)
	}
}

func TestLaunchdStatus(t *testing.T) {
	l := &launchd{label: "com.floscom.fleet", dir: t.TempDir(), domain: "gui/501"}
	if err := writeFile(l.Path(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	fakeRun(t, map[string]string{
		"launchctl print gui/501/com.floscom.fleet": "gui/501/com.floscom.fleet = {\n\tactive count = 1\n\tstate = running\n\tpid = 777\n\tendpoints = {\n\t\tstate = waiting\n\t}\n}",
		"launchctl print-disabled gui/501":          "disabled services = {\n\t\"com.floscom.fleet\" => enabled\n\t\"com.other\" => disabled\n}",
	}, nil)
	st, err := l.Status()
	want := Status{Installed: true, Enabled: true, Running: true, Pid: 777, Detail: "running"}
	if err != nil || st != want {
		t.Errorf("Status = %+v, %v; want %+v", st, err, want)
	}
}

func TestCleanPath(t *testing.T) {
	dir := t.TempDir()
	got := cleanPath(strings.Join([]string{dir, "/usr/bin", dir + "/", "relative", filepath.Join(dir, "gone"), "/usr/bin", ""}, ":"))
	if want := dir + ":/usr/bin"; got != want {
		t.Errorf("cleanPath = %q, want %q", got, want)
	}
}
