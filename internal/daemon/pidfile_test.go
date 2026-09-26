package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockPIDFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	unlock, err := lockPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockPIDFile(path); err == nil {
		t.Fatal("second lock succeeded while the first is held")
	}
	// A daemon that opened the file before the first one exits must lock
	// the same inode a third daemon would find at path: the file stays.
	early, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	unlock()
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Fatalf("after unlock: %q, %v; want an empty, existing file", b, err)
	}
	unlock2, err := lockPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock2()
	if !samePath(early, path) {
		t.Fatal("pid file was replaced")
	}
	// An inode that is no longer at path is detected.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if samePath(early, path) {
		t.Fatal("samePath true for an unlinked inode")
	}
}
