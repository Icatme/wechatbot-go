package persist

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.json")
	value := struct {
		Name string `json:"name"`
	}{Name: "current"}

	if err := WriteJSONAtomic(path, value); err != nil {
		t.Fatalf("write JSON atomically: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got, want := string(data), "{\n  \"name\": \"current\"\n}\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat result: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got&0077 != 0 {
		t.Fatalf("file permissions = %04o, want no group or other permissions", got)
	}
}

func TestWriteJSONAtomicMarshalFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := []byte("{\"name\":\"old\"}\n")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	err := WriteJSONAtomic(path, make(chan int))
	if err == nil {
		t.Fatal("expected marshal failure")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read old file: %v", readErr)
	}
	if string(data) != string(old) {
		t.Fatalf("file changed after failed write: %q", data)
	}
}

func TestWriteJSONAtomicRenameFailureCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}

	err := WriteJSONAtomic(path, map[string]string{"name": "new"})
	if err == nil {
		t.Fatal("expected rename failure")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read parent directory: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state.json.tmp-") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}
