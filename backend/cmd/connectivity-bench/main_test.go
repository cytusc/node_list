package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivateAtomicOutputDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "result.json")
	if err := writeJSON(path, map[string]int{"count": 512}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows maps mode bits loosely; the 0600 assertion is POSIX-only.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("output is not private")
	}
	before, _ := os.ReadFile(path)
	if err = writeJSON(path, map[string]int{"count": 1}); err == nil {
		t.Fatal("existing report overwritten")
	}
	link := filepath.Join(root, "symlink.json")
	if err = os.Symlink(path, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs privileges on windows")
		}
		t.Fatal(err)
	}
	if err = writeJSON(link, map[string]int{"count": 1}); err == nil {
		t.Fatal("followed symlink")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("report changed")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("temporary output leaked")
	}
}
