package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// machine is one store with its own folders, as on one machine.
type machine struct {
	*Store
	root string
	cli  []string // the agent CLIs' skills folders
}

func newMachine(t *testing.T, id string) *machine {
	t.Helper()
	root := t.TempDir()
	m := &machine{root: root, cli: []string{filepath.Join(root, "claude", "skills"), filepath.Join(root, "agents", "skills")}}
	s, err := Open(Options{
		Dir: filepath.Join(root, "fleet", "skills"), State: filepath.Join(root, "fleet", "skills.json"),
		ID: id, Name: "host-" + id, LinkDirs: func(context.Context) []string { return m.cli },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Store = s
	return m
}

// settle scans twice: a change is taken once it held still for a scan.
func (m *machine) settle(t *testing.T) {
	t.Helper()
	m.Scan(context.Background())
	m.Scan(context.Background())
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func entry(t *testing.T, m *machine, name string) (Entry, bool) {
	t.Helper()
	for _, e := range m.Manifest() {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// pull does what the dashboard's sync does between two daemons.
func pull(t *testing.T, to, from *machine) {
	t.Helper()
	for _, e := range to.Want(from.Manifest()) {
		var data []byte
		if to.Needs(e) {
			got, d, err := from.Archive(e.Name)
			if err != nil {
				t.Fatalf("archive %s: %v", e.Name, err)
			}
			e, data = got, d
		}
		if _, err := to.Apply(e, data); err != nil {
			t.Fatalf("apply %s: %v", e.Name, err)
		}
	}
}

func TestScanTakesSettledSkillsAndLeavesJunkOut(t *testing.T) {
	a := newMachine(t, "a")
	d := a.Dir()
	writeFile(t, filepath.Join(d, "deploy", "SKILL.md"), "# deploy", 0o644)
	writeFile(t, filepath.Join(d, "deploy", "scripts", "run.sh"), "#!/bin/sh", 0o755)
	writeFile(t, filepath.Join(d, "deploy", ".git", "HEAD"), "ref", 0o644)
	writeFile(t, filepath.Join(d, "deploy", ".DS_Store"), "x", 0o644)
	writeFile(t, filepath.Join(d, "deploy", "notes.md~"), "x", 0o644)
	writeFile(t, filepath.Join(d, "deploy.bak.20260722", "SKILL.md"), "# old", 0o644)
	writeFile(t, filepath.Join(d, "draft", "README.md"), "no skill yet", 0o644)

	a.Scan(context.Background())
	if len(a.Manifest()) != 0 {
		t.Fatalf("taken before it held still: %+v", a.Manifest())
	}
	a.Scan(context.Background())
	got := a.Manifest()
	if len(got) != 1 || got[0].Name != "deploy" || got[0].By != "a" || got[0].Host != "host-a" {
		t.Fatalf("manifest = %+v", got)
	}
	files, err := walk(filepath.Join(d, "deploy"))
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range files {
		rels = append(rels, f.rel)
	}
	if !slices.Equal(rels, []string{"SKILL.md", "scripts/run.sh"}) {
		t.Fatalf("files = %v", rels)
	}
	// Junk changing changes nothing.
	v := got[0].Version
	writeFile(t, filepath.Join(d, "deploy", ".git", "HEAD"), "other", 0o644)
	a.settle(t)
	if e, _ := entry(t, a, "deploy"); e.Version != v {
		t.Fatalf("junk bumped the version")
	}
}

func TestSyncChangesAndDeletesBothWays(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# deploy", 0o644)
	writeFile(t, filepath.Join(a.Dir(), "deploy", "scripts", "run.sh"), "#!/bin/sh\necho hi", 0o755)
	a.settle(t)

	pull(t, b, a)
	fi, err := os.Stat(filepath.Join(b.Dir(), "deploy", "scripts", "run.sh"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("script on b: %v %v", fi, err)
	}
	ea, _ := entry(t, a, "deploy")
	eb, _ := entry(t, b, "deploy")
	if ea != eb {
		t.Fatalf("a %+v, b %+v", ea, eb)
	}
	b.settle(t)
	if e, _ := entry(t, b, "deploy"); e != ea {
		t.Fatalf("b's scan made its own version of what came: %+v", e)
	}

	// Changed on b, back to a.
	writeFile(t, filepath.Join(b.Dir(), "deploy", "SKILL.md"), "# deploy v2", 0o644)
	b.settle(t)
	pull(t, a, b)
	if got, _ := os.ReadFile(filepath.Join(a.Dir(), "deploy", "SKILL.md")); string(got) != "# deploy v2" {
		t.Fatalf("a has %q", got)
	}
	if len(a.Want(b.Manifest())) != 0 || len(b.Want(a.Manifest())) != 0 {
		t.Fatal("not settled after one round")
	}

	// Deleted on a: gone from b, and not back from b.
	os.RemoveAll(filepath.Join(a.Dir(), "deploy"))
	a.settle(t)
	if e, _ := entry(t, a, "deploy"); !e.Deleted {
		t.Fatalf("no tombstone: %+v", e)
	}
	pull(t, a, b)
	pull(t, b, a)
	if _, err := os.Stat(filepath.Join(b.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatalf("still on b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatalf("back on a: %v", err)
	}
}

func TestSameFilesNeedNoTransfer(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	for _, m := range []*machine{a, b} {
		writeFile(t, filepath.Join(m.Dir(), "deploy", "SKILL.md"), "# deploy", 0o644)
		m.settle(t)
	}
	want := b.Want(a.Manifest())
	if len(want) == 1 && b.Needs(want[0]) {
		t.Fatal("same files fetched again")
	}
	for _, e := range want {
		if _, err := b.Apply(e, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnsettledChangeHereWinsOverIncoming(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# a", 0o644)
	a.settle(t)
	pull(t, b, a)
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# a2", 0o644)
	a.settle(t)

	// b edits too, not scanned yet, then a's newer change arrives.
	writeFile(t, filepath.Join(b.Dir(), "deploy", "SKILL.md"), "# b, just now", 0o644)
	pull(t, b, a)
	if got, _ := os.ReadFile(filepath.Join(b.Dir(), "deploy", "SKILL.md")); string(got) != "# b, just now" {
		t.Fatalf("b's edit was overwritten: %q", got)
	}
	pull(t, a, b)
	if got, _ := os.ReadFile(filepath.Join(a.Dir(), "deploy", "SKILL.md")); string(got) != "# b, just now" {
		t.Fatalf("a has %q", got)
	}
}

func TestLinks(t *testing.T) {
	a := newMachine(t, "a")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# deploy", 0o644)
	writeFile(t, filepath.Join(a.Dir(), "mine", "SKILL.md"), "# fleet's", 0o644)
	// The user's own skill by that name, and a broken link.
	writeFile(t, filepath.Join(a.cli[0], "mine", "SKILL.md"), "# the user's", 0o644)
	os.MkdirAll(a.cli[1], 0o755)
	os.Symlink(filepath.Join(a.root, "gone"), filepath.Join(a.cli[1], "deploy"))
	a.settle(t)

	for _, d := range a.cli {
		if got, err := os.ReadFile(filepath.Join(d, "deploy", "SKILL.md")); err != nil || string(got) != "# deploy" {
			t.Fatalf("%s: %q %v", d, got, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(a.cli[0], "mine", "SKILL.md")); string(got) != "# the user's" {
		t.Fatalf("the user's skill was replaced: %q", got)
	}
	info, err := Status(a.Dir(), a.opts.State, a.cli)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(info, func(in Info) bool { return in.Name == "mine" })
	if i < 0 || !slices.Equal(info[i].Conflicts, a.cli[:1]) || !slices.Equal(info[i].Links, a.cli[1:]) || info[i].Pending {
		t.Fatalf("status = %+v", info)
	}

	os.RemoveAll(filepath.Join(a.Dir(), "deploy"))
	a.Scan(context.Background())
	for _, d := range a.cli {
		if _, err := os.Lstat(filepath.Join(d, "deploy")); !os.IsNotExist(err) {
			t.Fatalf("link left in %s: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(a.cli[0], "mine")); err != nil {
		t.Fatal("the user's skill was removed")
	}
}

func TestLinkedSkillIsUpdatedWhereItLives(t *testing.T) {
	a, b := newMachine(t, "a"), newMachine(t, "b")
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# a", 0o644)
	a.settle(t)
	repo := filepath.Join(b.root, "repo", "deploy")
	writeFile(t, filepath.Join(repo, "SKILL.md"), "# old", 0o644)
	if err := os.Symlink(repo, filepath.Join(b.Dir(), "deploy")); err != nil {
		t.Fatal(err)
	}
	// b's link came first: a's newer version replaces it in the repo.
	b.settle(t)
	pull(t, a, b)
	writeFile(t, filepath.Join(a.Dir(), "deploy", "SKILL.md"), "# a2", 0o644)
	a.settle(t)
	pull(t, b, a)
	if got, _ := os.ReadFile(filepath.Join(repo, "SKILL.md")); string(got) != "# a2" {
		t.Fatalf("repo has %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(b.Dir(), "deploy")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}

	os.RemoveAll(filepath.Join(a.Dir(), "deploy"))
	a.settle(t)
	pull(t, b, a)
	if _, err := os.Lstat(filepath.Join(b.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatal("link still there")
	}
	if _, err := os.Stat(filepath.Join(repo, "SKILL.md")); err != nil {
		t.Fatal("the linked folder itself was deleted")
	}
}

func TestApplyRefusesBadArchives(t *testing.T) {
	b := newMachine(t, "b")
	tarOf := func(name string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	for _, name := range []string{"../evil", "/etc/evil", "a/../../evil"} {
		e := Entry{Name: "deploy", Hash: "h", Version: 1, By: "a"}
		if _, err := b.Apply(e, tarOf(name)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := b.Apply(Entry{Name: "deploy", Hash: "h", Version: 1, By: "a"}, tarOf("SKILL.md")); err == nil {
		t.Fatal("wrong hash accepted")
	}
	if _, err := b.Apply(Entry{Name: "deploy", Hash: "h", Version: 1, By: "a"}, nil); !errors.Is(err, ErrNeedData) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir(), "deploy")); !os.IsNotExist(err) {
		t.Fatal("a refused skill was installed")
	}
	if _, err := b.Apply(Entry{Name: "../x", Deleted: true, Version: 1, By: "a"}, nil); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestAdd(t *testing.T) {
	a := newMachine(t, "a")
	readSkill := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(p, "SKILL.md"))
		return string(b)
	}

	// In a CLI's folder, linked from the other: moved, both links go.
	inCLI := filepath.Join(a.cli[1], "deploy")
	writeFile(t, filepath.Join(inCLI, "SKILL.md"), "# deploy", 0o644)
	os.MkdirAll(a.cli[0], 0o755)
	os.Symlink("../../agents/skills/deploy", filepath.Join(a.cli[0], "deploy"))
	name, how, err := Add(a.Dir(), filepath.Join(a.cli[0], "deploy"), a.cli, false)
	if err != nil || name != "deploy" || how != Moved {
		t.Fatalf("add = %s %s %v", name, how, err)
	}
	if readSkill(filepath.Join(a.Dir(), "deploy")) != "# deploy" {
		t.Fatal("not in the shared folder")
	}
	for _, d := range a.cli {
		if _, err := os.Lstat(filepath.Join(d, "deploy")); !os.IsNotExist(err) {
			t.Fatalf("%s still has it: %v", d, err)
		}
	}
	a.settle(t)
	for _, d := range a.cli {
		if readSkill(filepath.Join(d, "deploy")) != "# deploy" {
			t.Fatalf("%s: not linked", d)
		}
	}

	// Elsewhere: copied, junk left behind, a CLI's link to it replaced.
	repo := filepath.Join(a.root, "repo", "review")
	writeFile(t, filepath.Join(repo, "SKILL.md"), "# review", 0o644)
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref", 0o644)
	os.Symlink(repo, filepath.Join(a.cli[0], "review"))
	if _, how, err := Add(a.Dir(), repo, a.cli, false); err != nil || how != Copied {
		t.Fatalf("add = %s %v", how, err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir(), "review", ".git")); !os.IsNotExist(err) {
		t.Fatal("junk copied")
	}
	if _, err := os.Stat(filepath.Join(repo, "SKILL.md")); err != nil {
		t.Fatal("source gone")
	}
	if _, err := os.Lstat(filepath.Join(a.cli[0], "review")); !os.IsNotExist(err) {
		t.Fatal("the link to the source stayed")
	}

	// With --link: it stays, and so do links to it.
	other := filepath.Join(a.root, "repo", "lint")
	writeFile(t, filepath.Join(other, "SKILL.md"), "# lint", 0o644)
	os.Symlink(other, filepath.Join(a.cli[0], "lint"))
	if _, how, err := Add(a.Dir(), other, a.cli, true); err != nil || how != Linked {
		t.Fatalf("add = %s %v", how, err)
	}
	if to, _ := os.Readlink(filepath.Join(a.Dir(), "lint")); to != other {
		t.Fatalf("link to %q", to)
	}
	if ok, _ := linkState(a.Dir(), a.cli[0], "lint"); !ok {
		t.Fatal("the CLI's link does not count as linked")
	}

	// Refused: shared already, not a skill, a bad name.
	if _, _, err := Add(a.Dir(), repo, a.cli, false); err == nil {
		t.Fatal("added twice")
	}
	os.MkdirAll(filepath.Join(a.root, "empty"), 0o755)
	if _, _, err := Add(a.Dir(), filepath.Join(a.root, "empty"), a.cli, false); err == nil {
		t.Fatal("added a folder without SKILL.md")
	}
	writeFile(t, filepath.Join(a.root, "x.bak", "SKILL.md"), "#", 0o644)
	if _, _, err := Add(a.Dir(), filepath.Join(a.root, "x.bak"), a.cli, false); err == nil {
		t.Fatal("added a bad name")
	}

	// Removing a linked skill leaves its folder.
	if err := Remove(a.Dir(), "lint"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "SKILL.md")); err != nil {
		t.Fatal("the linked folder was deleted")
	}
	if err := Remove(a.Dir(), "lint"); err == nil {
		t.Fatal("removed twice")
	}
}
