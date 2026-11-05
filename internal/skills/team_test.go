package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// tarFiles packs files as a publisher's gateway sends them, junk and all.
func tarFiles(t *testing.T, files ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < len(files); i += 2 {
		name, data := files[i], files[i+1]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func teamSrc(bundle, owner string, version int64) Source {
	return Source{Team: "t1", TeamName: "codeline", Bundle: bundle, BundleVersion: version, Owner: "id-" + owner, OwnerName: owner}
}

func installTeam(t *testing.T, m *machine, src Source, name string, data []byte) string {
	t.Helper()
	got, err := m.InstallTeam(context.Background(), src, name, data)
	if err != nil {
		t.Fatalf("install %s: %v", name, err)
	}
	return got
}

func readSkillMD(t *testing.T, m *machine, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.Dir(), name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallTeamAndUpdateInPlace(t *testing.T) {
	a := newMachine(t, "a")
	a.Scan(context.Background())
	v1 := tarFiles(t, "SKILL.md", "---\nname: git-commit\n---\n# v1", ".DS_Store", "x", "scripts/run.sh", "#!/bin/sh")
	if got := installTeam(t, a, teamSrc("b1", "flo", 1), "git-commit", v1); got != "git-commit" {
		t.Fatalf("installed as %s", got)
	}
	e, _ := entry(t, a, "git-commit")
	if e.Source == nil || e.Source.Hash != e.Hash || e.Source.Name != "git-commit" || e.Source.Bundle != "b1" || e.By != "a" || e.ForkedFrom != nil {
		t.Fatalf("entry = %+v %+v", e, e.Source)
	}
	if got, err := os.ReadFile(filepath.Join(a.cli[0], "git-commit", "SKILL.md")); err != nil || string(got) != "---\nname: git-commit\n---\n# v1" {
		t.Fatalf("not linked: %q %v", got, err)
	}
	// Junk that came along is no change made here.
	a.settle(t)
	if got, _ := entry(t, a, "git-commit"); got.Version != e.Version || got.Source == nil {
		t.Fatalf("the scan took the install for a change: %+v", got)
	}

	// An update keeps the name and gets a new version.
	v2 := tarFiles(t, "SKILL.md", "---\nname: git-commit\n---\n# v2")
	if got := installTeam(t, a, teamSrc("b1", "flo", 2), "git-commit", v2); got != "git-commit" {
		t.Fatalf("updated as %s", got)
	}
	e2, _ := entry(t, a, "git-commit")
	if readSkillMD(t, a, "git-commit") != "---\nname: git-commit\n---\n# v2" || e2.Version <= e.Version || e2.Source.BundleVersion != 2 || e2.Source.Hash != e2.Hash {
		t.Fatalf("entry = %+v %+v", e2, e2.Source)
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "git-commit", "scripts")); !os.IsNotExist(err) {
		t.Fatal("v1's files stayed")
	}
	// The same again, or an older version: nothing changes.
	installTeam(t, a, teamSrc("b1", "flo", 2), "git-commit", v2)
	installTeam(t, a, teamSrc("b1", "flo", 1), "git-commit", v1)
	if got, _ := entry(t, a, "git-commit"); got.Version != e2.Version || readSkillMD(t, a, "git-commit") != "---\nname: git-commit\n---\n# v2" {
		t.Fatalf("entry = %+v", got)
	}
	if got := a.TeamSkills("t1"); len(got) != 1 || got[0].Name != "git-commit" || len(a.TeamSkills("t2")) != 0 {
		t.Fatalf("team skills = %+v", got)
	}

	// Bad archives install nothing.
	for _, data := range [][]byte{tarFiles(t, "SKILL.md", "#", "../evil", "x"), tarFiles(t, "README.md", "no skill"), []byte("not a tar")} {
		if _, err := a.InstallTeam(context.Background(), teamSrc("bad", "flo", 1), "bad", data); err == nil {
			t.Fatal("bad archive accepted")
		}
	}
	if _, ok := entry(t, a, "bad"); ok {
		t.Fatal("a refused skill is in the manifest")
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "bad")); !os.IsNotExist(err) {
		t.Fatal("a refused skill was installed")
	}
	if _, err := a.InstallTeam(context.Background(), Source{Team: "t1"}, "x", v1); err == nil {
		t.Fatal("installed without a bundle")
	}
}

func TestInstallTeamNameConflicts(t *testing.T) {
	a := newMachine(t, "a")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# mine", 0o644)
	writeFile(t, filepath.Join(a.cli[0], "review", "SKILL.md"), "# the user's", 0o644)
	a.settle(t)
	skill := func(name string) []byte {
		return tarFiles(t, "SKILL.md", "---\nname: "+name+"\ndescription: d\n---\n# "+name)
	}
	installTeam(t, a, teamSrc("b0", "ana", 1), "lint", skill("lint"))

	for _, c := range []struct{ bundle, owner, name, want string }{
		{"b1", "flo", "deploy", "flo-deploy"},   // own skill in the shared folder
		{"b2", "flo", "review", "flo-review"},   // the user's own in a CLI's folder
		{"b3", "flo", "lint", "flo-lint"},       // another member's team skill
		{"b4", "flo", "deploy", "flo-deploy-2"}, // taken twice
		{"b5", "Jürgen M.", "deploy", "J-rgen-M-deploy"},
		{"b6", "flo", "bad name!", "bad-name"},
	} {
		got := installTeam(t, a, teamSrc(c.bundle, c.owner, 1), c.name, skill(c.name))
		if got != c.want {
			t.Fatalf("%s/%s installed as %s, want %s", c.owner, c.name, got, c.want)
		}
		if want := "---\nname: \"" + c.want + "\"\ndescription: d\n---\n# " + c.name; readSkillMD(t, a, got) != want {
			t.Fatalf("%s: SKILL.md = %q", got, readSkillMD(t, a, got))
		}
		if e, _ := entry(t, a, got); e.Source.Name != c.name || e.Source.Hash != e.Hash {
			t.Fatalf("%s: %+v", got, e.Source)
		}
	}
	if readSkillMD(t, a, "deploy") != "# mine" || readSkillMD(t, a, "lint") != "---\nname: lint\ndescription: d\n---\n# lint" {
		t.Fatal("a taken skill was replaced")
	}
	if got, _ := os.ReadFile(filepath.Join(a.cli[0], "review", "SKILL.md")); string(got) != "# the user's" {
		t.Fatalf("the user's skill was replaced: %q", got)
	}
	// The renames are no changes made here.
	before := a.Manifest()
	a.settle(t)
	if !slices.EqualFunc(before, a.Manifest(), func(x, y Entry) bool { return x.Version == y.Version && x.Source != nil == (y.Source != nil) }) {
		t.Fatalf("the scan changed %+v", a.Manifest())
	}
	// An update keeps the name it got.
	if got := installTeam(t, a, teamSrc("b1", "flo", 2), "deploy", skill("deploy")); got != "flo-deploy" {
		t.Fatalf("updated as %s", got)
	}
}

func TestSetName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"# deploy\nname: deploy\n", "# deploy\nname: deploy\n"},
		{"---\nname: deploy\ndescription: Ship it\n---\n# body\n", "---\nname: \"x-1\"\ndescription: Ship it\n---\n# body\n"},
		{"---\r\nname: deploy\r\ndescription: d\r\n---\r\nbody\r\n", "---\r\nname: \"x-1\"\r\ndescription: d\r\n---\r\nbody\r\n"},
		{"---\nname: \"deploy\" # it\n---\n", "---\nname: \"x-1\"\n---\n"},
		{"---\n'name' : 'it''s'\n---\n", "---\nname: \"x-1\"\n---\n"},
		{"---\ndescription: d\n---\n", "---\nname: \"x-1\"\ndescription: d\n---\n"},
		{"---\nname: >\n  de\n\n  ploy\n\ndescription: d\n---\n", "---\nname: \"x-1\"\n\ndescription: d\n---\n"},
		{"---\nmetadata:\n  name: other\nnamespace: n\nname: deploy\n---\n", "---\nmetadata:\n  name: other\nnamespace: n\nname: \"x-1\"\n---\n"},
		{"\ufeff---\nname: deploy\n---\nname: body\n", "\ufeff---\nname: \"x-1\"\n---\nname: body\n"},
		{"---\nname: deploy\n...\n", "---\nname: \"x-1\"\n...\n"},
		{"---\nname: deploy\nno end\n", "---\nname: deploy\nno end\n"},
		{"--- x\nname: deploy\n---\n", "--- x\nname: deploy\n---\n"},
		{"", ""},
	} {
		if got := setName(c.in, "x-1"); got != c.want {
			t.Errorf("setName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTeamSkillForksOnChangeOnly(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	data := tarFiles(t, "SKILL.md", "---\nname: deploy\n---\n# team's")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# a's own", 0o644)
	a.settle(t)
	name := installTeam(t, a, teamSrc("b1", "flo", 1), "deploy", data)

	// Synced to b, through JSON as between daemons: no fork there.
	for _, e := range b.Want(a.Manifest()) {
		got, d, err := a.Archive(e.Name)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := json.Marshal(got)
		var e2 Entry
		if err := json.Unmarshal(j, &e2); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Apply(e2, d); err != nil {
			t.Fatal(err)
		}
	}
	b.settle(t)
	ea, _ := entry(t, a, name)
	eb, _ := entry(t, b, name)
	if eb.Source == nil || *eb.Source != *ea.Source || eb.Version != ea.Version || eb.By != "a" {
		t.Fatalf("on b: %+v %+v", eb, eb.Source)
	}
	if len(a.Want(b.Manifest())) != 0 {
		t.Fatal("b made its own version of what came")
	}
	// A daemon older than teams drops the source, and that is no news.
	var old []struct {
		Name, Hash, By string
		Size, Version  int64
	}
	j, _ := json.Marshal(b.Manifest())
	json.Unmarshal(j, &old)
	var stripped []Entry
	for _, o := range old {
		stripped = append(stripped, Entry{Name: o.Name, Hash: o.Hash, Size: o.Size, Version: o.Version, By: o.By})
	}
	if len(a.Want(stripped)) != 0 {
		t.Fatal("an older daemon's manifest replaces a team skill")
	}

	// Changed on b: forked, and so on a too.
	writeFile(t, filepath.Join(b.Dir(), name, "SKILL.md"), "---\nname: flo-deploy\n---\n# b's", 0o644)
	b.settle(t)
	eb, _ = entry(t, b, name)
	if eb.Source != nil || eb.ForkedFrom == nil || eb.ForkedFrom.Ref() != "flo/deploy" || eb.By != "b" {
		t.Fatalf("not forked on b: %+v", eb)
	}
	pull(t, a, b)
	ea, _ = entry(t, a, name)
	if ea.Source != nil || ea.ForkedFrom == nil {
		t.Fatalf("not forked on a: %+v", ea)
	}
	if _, err := a.InstallTeam(context.Background(), teamSrc("b1", "flo", 2), "deploy", data); !errors.Is(err, ErrForked) {
		t.Fatalf("updated a fork: %v", err)
	}
	if err := a.RemoveTeam("t1", "b1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed a fork: %v", err)
	}
	if readSkillMD(t, a, name) != "---\nname: flo-deploy\n---\n# b's" {
		t.Fatal("the fork was replaced")
	}
	if got := a.TeamSkills(""); len(got) != 1 || got[0].Name != name {
		t.Fatalf("team skills = %+v", got)
	}
	// A fork changed again stays one.
	writeFile(t, filepath.Join(a.Dir(), name, "SKILL.md"), "# a's now", 0o644)
	a.settle(t)
	if e, _ := entry(t, a, name); e.ForkedFrom == nil || e.Source != nil {
		t.Fatalf("entry = %+v", e)
	}

	info, err := Status(a.Dir(), a.opts.State, a.cli)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(info, func(in Info) bool { return in.Name == name }); i < 0 || info[i].Entry.ForkedFrom == nil {
		t.Fatalf("status = %+v", info)
	}
}

func TestChangeNotTakenYetForksBeforeAnUpdate(t *testing.T) {
	a := newMachine(t, "a")
	installTeam(t, a, teamSrc("b1", "flo", 1), "deploy", tarFiles(t, "SKILL.md", "# v1"))
	a.settle(t)
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# mine, just now", 0o644)
	if _, err := a.InstallTeam(context.Background(), teamSrc("b1", "flo", 2), "deploy", tarFiles(t, "SKILL.md", "# v2")); !errors.Is(err, ErrForked) {
		t.Fatalf("err = %v", err)
	}
	if readSkillMD(t, a, "deploy") != "# mine, just now" {
		t.Fatal("the edit was overwritten")
	}
}

func TestRemoveTeam(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	installTeam(t, a, teamSrc("b1", "flo", 1), "deploy", tarFiles(t, "SKILL.md", "# deploy"))
	a.settle(t)
	pull(t, b, a)
	if err := a.RemoveTeam("t1", "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
	e, _ := entry(t, a, "deploy")
	if !e.Deleted || e.Source == nil || e.Source.Bundle != "b1" {
		t.Fatalf("tombstone = %+v", e)
	}
	pull(t, b, a)
	if _, err := os.Stat(filepath.Join(b.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatal("still on b")
	}
	if eb, _ := entry(t, b, "deploy"); !eb.Deleted || eb.Source == nil {
		t.Fatalf("tombstone on b = %+v", eb)
	}
	if err := a.RemoveTeam("t1", "b1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if got := a.TeamSkills("t1"); len(got) != 1 || !got[0].Deleted {
		t.Fatalf("team skills = %+v", got)
	}
	// Subscribed again: back under its name.
	if got := installTeam(t, a, teamSrc("b1", "flo", 2), "deploy", tarFiles(t, "SKILL.md", "# deploy")); got != "deploy" {
		t.Fatalf("installed as %s", got)
	}

	// Deleted by the user: the tombstone says which team skill it was.
	os.RemoveAll(filepath.Join(a.Dir(), "deploy"))
	a.settle(t)
	if e, _ := entry(t, a, "deploy"); !e.Deleted || e.Source == nil || e.Source.BundleVersion != 2 {
		t.Fatalf("tombstone = %+v", e)
	}
}
