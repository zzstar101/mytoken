package cli

import (
	"strings"
	"testing"
)

func TestTableAlignsWideText(t *testing.T) {
	var b strings.Builder
	w := newTable(&b)
	w.Write([]byte("NAME\tCOST\tNOTE\n米醋API\t1.00\tok\nabc\t22.50\t-\n\nafter\n"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "NAME     COST   NOTE\n米醋API  1.00   ok\nabc      22.50  -\n\nafter\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}
