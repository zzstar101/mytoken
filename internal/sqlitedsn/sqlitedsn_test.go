package sqlitedsn

import (
	"runtime"
	"testing"
)

func TestURI(t *testing.T) {
	if got := URI("/a b/x.db", "mode=ro"); runtime.GOOS != "windows" && got != "file:///a%20b/x.db?mode=ro" {
		t.Fatalf("got %q", got)
	}
	if runtime.GOOS == "windows" {
		if got := URI(`C:\Users\me\x.db`, ""); got != "file:///C:/Users/me/x.db" {
			t.Fatalf("got %q", got)
		}
	}
}
