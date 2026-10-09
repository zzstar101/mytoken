// Package sqlitedsn builds SQLite file: URIs that work on every platform.
package sqlitedsn

import (
	"net/url"
	"path/filepath"
	"strings"
)

// URI returns a file: URI for the database at path with the given query.
// Windows paths become file:///C:/dir/db, which SQLite reads as the drive
// path; a bare file://C:\... is rejected as an "invalid uri authority".
func URI(path, rawQuery string) string {
	p := filepath.ToSlash(path)
	if vol := filepath.VolumeName(path); vol != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: rawQuery}).String()
}
