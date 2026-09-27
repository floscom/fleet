package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunArgv(t *testing.T) {
	d := &Docker{Binary: "/usr/bin/docker"}
	argv := d.RunArgv(RunSpec{
		Name:      "fleet-a1",
		Image:     "img:1",
		Labels:    map[string]string{LabelHome: "/h", LabelAgent: "a1"},
		User:      "1000:1000",
		Workdir:   "/w/sub",
		Mounts:    []Mount{{Source: "/w", Target: "/w"}, {Source: "/s", Target: "/s", ReadOnly: true}},
		EnvFile:   "/s/docker.env",
		Network:   "none",
		ExtraArgs: []string{"--memory=1g"},
		Argv:      []string{"claude", "--", "-p"},
	})
	want := []string{"/usr/bin/docker", "run", "--rm", "-it", "--init",
		"--name", "fleet-a1", "--detach-keys", DetachKeys, "--pull", "never", "--cap-drop", "ALL",
		"--label", "fleet.agent=a1", "--label", "fleet.home=/h",
		"--user", "1000:1000", "--workdir", "/w/sub", "--env-file", "/s/docker.env", "--network", "none",
		"--mount", "type=bind,source=/w,target=/w", "--mount", "type=bind,source=/s,target=/s,readonly",
		"--memory=1g", "img:1", "claude", "--", "-p"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", argv, want)
	}
}

func TestMountFlagQuoting(t *testing.T) {
	m := Mount{Source: `/a,b/"c"`, Target: "/t", ReadOnly: true}
	if got, want := m.flag(), `type=bind,"source=/a,b/""c""",target=/t,readonly`; got != want {
		t.Fatalf("flag = %s, want %s", got, want)
	}
}

func TestWriteEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	skipped, err := WriteEnvFile(p, map[string]string{"B": "2 two", "A": "x=y", "BAD": "a\nb", "": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(skipped, []string{"", "BAD"}) {
		t.Errorf("skipped = %q", skipped)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "A=x=y\nB=2 two\n" {
		t.Errorf("env file = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
}

func TestDockerfileEmbedded(t *testing.T) {
	if !strings.Contains(string(Dockerfile), "\nFROM ") {
		t.Fatal("Dockerfile not embedded")
	}
}
