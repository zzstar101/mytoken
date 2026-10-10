package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/store"
)

func TestWatchDirs(t *testing.T) {
	root := filepath.FromSlash("/h/.hermes")
	got := watchDirs([]string{root, ""}, []string{
		filepath.FromSlash("/h/.hermes"),
		filepath.FromSlash("/h/.hermes/profiles/work"),
		filepath.FromSlash("/elsewhere/logs"),
	})
	want := []string{
		root,
		filepath.FromSlash("/elsewhere/logs"),
		filepath.FromSlash("/h/.hermes/profiles"),
		filepath.FromSlash("/h/.hermes/profiles/work"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watchDirs = %v, want %v", got, want)
	}
}

// A harness home can hold a whole program: Run must not watch it, or the
// app runs out of file descriptors (one per watched file on macOS).
func TestRunDoesNotWatchWholeRoot(t *testing.T) {
	root := t.TempDir()
	for i := range 50 {
		dir := filepath.Join(root, "program", "node_modules", fmt.Sprintf("pkg%d", i), "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "log"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, nil, nil, testParser{root: root})
	s.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var watching int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		watching = s.watching
		ready := s.dirs != nil
		s.mu.Unlock()
		if ready && watching > 0 {
			break
		}
	}
	cancel()
	<-done
	if watching != 1 {
		t.Fatalf("Run watches %d directories, want only the root", watching)
	}
}
