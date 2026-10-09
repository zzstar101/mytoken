// Package harnesstest is the conformance suite every harness parser must pass
// (see docs/HARNESS.md). It runs a parser against a small, redacted fixture and
// checks the properties the scanner depends on:
//
//   - golden: Discover+Parse output matches a checked-in golden JSON file
//     (rewrite it with `go test ./internal/harness/<harness> -run Conformance -update`).
//   - incremental: growing the fixture in steps and parsing each step with the
//     previous Next cursor yields the same events and sessions as one full parse.
//   - cursor-stable: parsing an unchanged source again emits no events and
//     leaves the cursor untouched.
//   - idempotent: two parses from a zero cursor produce the same DedupKey set,
//     with no duplicate key inside a single parse.
//   - privacy: the fixture's conversation text (SentinelText) never reaches an
//     event or session field.
//   - discover-missing: a root that does not exist yields no sources and no error.
//
// A harness wires itself up in one file:
//
//	func TestConformance(t *testing.T) {
//		harnesstest.Run(t, harnesstest.Case{
//			Name:    "claude-code",
//			New:     func(root string) harness.Parser { return claude.NewWithRoots(root) },
//			Fixture: "testdata/conformance/claude",
//		})
//	}
//
//	func BenchmarkConformance(b *testing.B) {
//		harnesstest.Bench(b, harnesstest.Case{ /* same fields */ })
//	}
package harnesstest

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// SentinelText is a word that must never reach an event or session field. Put
// it in a fixture's assistant reply, tool output or reasoning text — never in
// the first user message, because that one becomes SessionMeta.Title.
const SentinelText = "SENTINEL_PRIVATE_TEXT"

// Checks lists every check Run executes, in order. Use Case.Skip to opt out.
var Checks = []string{"golden", "incremental", "cursor-stable", "idempotent", "privacy", "discover-missing"}

// RootPlaceholder is the conventional stand-in for a fixture's absolute root
// when a Case.Rewrite normalizes path-derived fields.
const RootPlaceholder = "<ROOT>"

// Case describes one harness's conformance fixture.
type Case struct {
	// Name is the harness id (model.Harness); it names the subtests.
	Name string
	// New builds a parser rooted at root, normally the harness's NewWithRoots.
	New func(root string) harness.Parser
	// Fixture is the checked-in fixture directory, relative to the harness
	// package, e.g. "testdata/conformance/claude".
	Fixture string
	// Golden is the golden file; it defaults to <Fixture>/golden.json. The file
	// is written by `-update` and must never be copied into the parsed root.
	Golden string
	// Rewrite normalizes a snapshot after parsing and before it is compared or
	// written to the golden file. Set it when parser output legitimately depends
	// on the fixture's absolute path — for example a project path derived from
	// the database file's location (crush) — and replace that path with
	// RootPlaceholder so the golden is reproducible on any machine. The fixture
	// root differs between the baseline and the incremental steps, so without
	// normalization those checks cannot be equal.
	Rewrite func(root string, snap *Snapshot)
	// Steps is the number of growth steps the incremental and cursor checks
	// use. Default 3.
	Steps int
	// Grow advances the mutable fixture copy in dir to step (0-based). When nil
	// the suite copies whole lines of every fixture file progressively, which
	// suits append-only JSONL logs. Set it for sources that are not appended to:
	// insert another slice of rows into a sqlite database, or rewrite a JSON
	// document with more entries.
	Grow func(t testing.TB, dir string, step int) error
	// Skip names checks to skip (see Checks). Skipping is a last resort: record
	// why in a comment next to the Case.
	Skip map[string]bool
}

// Source is one discovered source, relative to the fixture root.
type Source struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Snapshot is the golden document: everything a full parse produced.
type Snapshot struct {
	Harness  string              `json:"harness"`
	Sources  []Source            `json:"sources"`
	Events   []model.UsageEvent  `json:"events"`
	Sessions []model.SessionMeta `json:"sessions"`
}

var update = flag.Bool("update", false, "rewrite conformance golden files")

// Run executes every check in Checks (minus Case.Skip) as a subtest.
func Run(t *testing.T, c Case) {
	t.Helper()
	c = c.withDefaults(t)
	for _, name := range Checks {
		if c.Skip[name] {
			continue
		}
		fn, ok := checkFns[name]
		if !ok {
			t.Fatalf("harnesstest: unknown check %q", name)
		}
		t.Run(name, func(t *testing.T) { fn(t, c) })
	}
}

var checkFns = map[string]func(*testing.T, Case){
	"golden":           checkGolden,
	"incremental":      checkIncremental,
	"cursor-stable":    checkCursorStable,
	"idempotent":       checkIdempotent,
	"privacy":          checkPrivacy,
	"discover-missing": checkDiscoverMissing,
}

// Materialize copies the fixture into a fresh temp directory and returns its
// path. With Case.Grow set, the fixture is produced by running every growth
// step in order instead of copying files.
func Materialize(t testing.TB, c Case) string {
	t.Helper()
	c = c.withDefaults(t)
	dir := t.TempDir()
	if c.Grow != nil {
		for step := 0; step < c.Steps; step++ {
			if err := c.Grow(t, dir, step); err != nil {
				t.Fatalf("harnesstest: Grow(step %d): %v", step, err)
			}
		}
		return dir
	}
	if err := copyTree(c.Fixture, dir, c.Golden); err != nil {
		t.Fatalf("harnesstest: copy fixture %s: %v", c.Fixture, err)
	}
	return dir
}

// Parse discovers every source under root and parses it, starting from the
// cursor recorded in cur (keyed by fixture-relative path, nil for a cold read).
// It returns the merged, sorted snapshot and the cursors to resume from.
func Parse(t testing.TB, c Case, root string, cur map[string]harness.Cursor) (Snapshot, map[string]harness.Cursor) {
	t.Helper()
	c = c.withDefaults(t)
	ctx := context.Background()
	p := c.New(root)
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("harnesstest: Discover(%s): %v", root, err)
	}
	sort.Slice(srcs, func(i, j int) bool { return filepath.ToSlash(srcs[i].Path) < filepath.ToSlash(srcs[j].Path) })
	snap := Snapshot{Harness: string(p.Harness())}
	next := make(map[string]harness.Cursor, len(srcs))
	for _, src := range srcs {
		rel := relPath(root, src.Path)
		snap.Sources = append(snap.Sources, Source{Path: rel, Kind: src.Kind})
		var from harness.Cursor
		if cur != nil {
			from = cur[rel]
		}
		b, err := p.Parse(ctx, src, from)
		if err != nil {
			t.Fatalf("harnesstest: Parse(%s): %v", rel, err)
		}
		snap.Events = append(snap.Events, b.Events...)
		snap.Sessions = append(snap.Sessions, b.Sessions...)
		next[rel] = b.Next
	}
	sort.Slice(snap.Events, func(i, j int) bool {
		if snap.Events[i].DedupKey != snap.Events[j].DedupKey {
			return snap.Events[i].DedupKey < snap.Events[j].DedupKey
		}
		return snap.Events[i].SessionID < snap.Events[j].SessionID
	})
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].SessionID < snap.Sessions[j].SessionID })
	if c.Rewrite != nil {
		c.Rewrite(root, &snap)
	}
	for i := range snap.Events {
		snap.Events[i].ProjectPath = slashRooted(snap.Events[i].ProjectPath)
	}
	for i := range snap.Sessions {
		snap.Sessions[i].Project = slashRooted(snap.Sessions[i].Project)
	}
	return snap, next
}

// slashRooted writes a path under RootPlaceholder with forward slashes, so a
// golden file is the same on Windows.
func slashRooted(p string) string {
	if rest, ok := strings.CutPrefix(p, RootPlaceholder); ok {
		return RootPlaceholder + filepath.ToSlash(rest)
	}
	return p
}

// Marshal renders a snapshot as the golden file's bytes.
func Marshal(s Snapshot) []byte {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic(err) // the snapshot is a plain struct; Marshal cannot fail
	}
	return append(b, '\n')
}

// Bench benchmarks a cold full parse of the fixture.
func Bench(b *testing.B, c Case) {
	c = c.withDefaults(b)
	root := Materialize(b, c)
	ctx := context.Background()
	p := c.New(root)
	srcs, err := p.Discover(ctx)
	if err != nil {
		b.Fatalf("harnesstest: Discover: %v", err)
	}
	sort.Slice(srcs, func(i, j int) bool { return filepath.ToSlash(srcs[i].Path) < filepath.ToSlash(srcs[j].Path) })
	var events int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events = 0
		for _, src := range srcs {
			batch, err := p.Parse(ctx, src, harness.Cursor{})
			if err != nil {
				b.Fatalf("harnesstest: Parse(%s): %v", src.Path, err)
			}
			events += len(batch.Events)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(events), "events/op")
	b.ReportMetric(float64(len(srcs)), "sources")
}

func (c Case) withDefaults(t testing.TB) Case {
	t.Helper()
	if c.Name == "" {
		t.Fatalf("harnesstest: Case.Name is required")
	}
	if c.New == nil {
		t.Fatalf("harnesstest: Case.New is required")
	}
	if c.Fixture == "" {
		t.Fatalf("harnesstest: Case.Fixture is required")
	}
	if c.Golden == "" {
		c.Golden = filepath.Join(c.Fixture, "golden.json")
	}
	if c.Steps <= 0 {
		c.Steps = 3
	}
	return c
}

func checkGolden(t *testing.T, c Case) {
	t.Helper()
	root := Materialize(t, c)
	snap, _ := Parse(t, c, root, nil)
	// A fixture that discovers nothing (wrong layout, wrong Case.New) would
	// otherwise pass every check by being consistently empty.
	if len(snap.Sources) == 0 {
		t.Fatalf("harnesstest: %s discovered no sources under %s — check the fixture layout and Case.New", c.Name, c.Fixture)
	}
	if len(snap.Events) == 0 {
		t.Fatalf("harnesstest: %s parsed no events from %s — a conformance fixture must contain usage records", c.Name, c.Fixture)
	}
	got := Marshal(snap)
	if *update {
		if err := os.MkdirAll(filepath.Dir(c.Golden), 0o755); err != nil {
			t.Fatalf("harnesstest: %v", err)
		}
		if err := os.WriteFile(c.Golden, got, 0o644); err != nil {
			t.Fatalf("harnesstest: %v", err)
		}
		t.Logf("harnesstest: rewrote %s", c.Golden)
		return
	}
	want, err := os.ReadFile(c.Golden)
	if err != nil {
		t.Fatalf("harnesstest: read golden %s: %v\n(run `go test ./internal/harness/%s -run Conformance -update`)", c.Golden, err, c.Name)
	}
	// A Windows checkout that converted line endings is still the same document.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(want, got) {
		t.Errorf("harnesstest: %s output does not match %s\n%s", c.Name, c.Golden, diffLines(want, got))
	}
}

func checkIncremental(t *testing.T, c Case) {
	t.Helper()
	base := Materialize(t, c)
	want, _ := Parse(t, c, base, nil)
	dir := t.TempDir()
	cur := map[string]harness.Cursor{}
	var got merger
	for step := 0; step < c.Steps; step++ {
		if err := grow(t, c, dir, step); err != nil {
			t.Fatalf("harnesstest: grow(step %d): %v", step, err)
		}
		snap, next := Parse(t, c, dir, cur)
		cur = next
		got.add(t, snap)
	}
	got.compare(t, want)
}

func checkCursorStable(t *testing.T, c Case) {
	t.Helper()
	dir := t.TempDir()
	cur := map[string]harness.Cursor{}
	for step := 0; step < c.Steps; step++ {
		if err := grow(t, c, dir, step); err != nil {
			t.Fatalf("harnesstest: grow(step %d): %v", step, err)
		}
		_, next := Parse(t, c, dir, cur)
		cur = next
	}
	snap, next := Parse(t, c, dir, cur)
	if len(snap.Events) != 0 {
		t.Errorf("harnesstest: an unchanged source re-emitted %d events (first %q)", len(snap.Events), snap.Events[0].DedupKey)
	}
	for rel, was := range cur {
		if next[rel] != was {
			t.Errorf("harnesstest: cursor for %s moved on an unchanged source:\n was %+v\n got %+v", rel, was, next[rel])
		}
	}
}

func checkIdempotent(t *testing.T, c Case) {
	t.Helper()
	root := Materialize(t, c)
	first, _ := Parse(t, c, root, nil)
	second, _ := Parse(t, c, root, nil)
	if d := duplicateKeys(first); len(d) > 0 {
		t.Errorf("harnesstest: one parse produced duplicate DedupKeys: %v", d)
	}
	if d := duplicateKeys(second); len(d) > 0 {
		t.Errorf("harnesstest: one parse produced duplicate DedupKeys: %v", d)
	}
	if got, want := keyList(second), keyList(first); !reflect.DeepEqual(got, want) {
		t.Errorf("harnesstest: re-parsing from a zero cursor changed the DedupKey set\n missing: %v\n extra:   %v", missing(want, got), missing(got, want))
	}
}

func checkPrivacy(t *testing.T, c Case) {
	t.Helper()
	if !treeContains(t, c.Fixture, SentinelText) {
		t.Errorf("harnesstest: fixture %s has no %s, so the privacy check proves nothing", c.Fixture, SentinelText)
	}
	root := Materialize(t, c)
	snap, _ := Parse(t, c, root, nil)
	if blob := Marshal(snap); bytes.Contains(blob, []byte(SentinelText)) {
		t.Errorf("harnesstest: private conversation text reached the parsed output:\n%s", contextAround(blob, SentinelText))
	}
	if raw, err := os.ReadFile(c.Golden); err == nil && bytes.Contains(raw, []byte(SentinelText)) {
		t.Errorf("harnesstest: the golden file %s contains %s", c.Golden, SentinelText)
	}
}

func checkDiscoverMissing(t *testing.T, c Case) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "missing-root")
	srcs, err := c.New(root).Discover(context.Background())
	if err != nil {
		t.Fatalf("harnesstest: Discover on a missing root returned an error: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("harnesstest: Discover on a missing root returned %d sources: %v", len(srcs), srcs)
	}
}

// grow materializes the state for step in dir: Case.Grow when set, otherwise
// whole-line prefixes of every fixture file (append-only logs).
func grow(t testing.TB, c Case, dir string, step int) error {
	t.Helper()
	if c.Grow != nil {
		return c.Grow(t, dir, step)
	}
	files, err := fixtureFiles(c.Fixture)
	if err != nil {
		return err
	}
	for _, rel := range files {
		lines, err := os.ReadFile(filepath.Join(c.Fixture, rel))
		if err != nil {
			return err
		}
		chunks := splitLines(lines)
		take := (len(chunks)*(step+1) + c.Steps - 1) / c.Steps
		if take > len(chunks) {
			take = len(chunks)
		}
		var buf bytes.Buffer
		for _, chunk := range chunks[:take] {
			buf.Write(chunk)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// splitLines splits data into chunks that each end on a newline, so a growth
// step never leaves a partial line behind.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			out = append(out, data)
			break
		}
		out = append(out, data[:i+1])
		data = data[i+1:]
	}
	return out
}

type merger struct {
	events   map[string]model.UsageEvent
	sessions map[string]model.SessionMeta
}

func (m *merger) add(t testing.TB, s Snapshot) {
	t.Helper()
	if m.events == nil {
		m.events = map[string]model.UsageEvent{}
		m.sessions = map[string]model.SessionMeta{}
	}
	for _, e := range s.Events {
		if prev, ok := m.events[e.DedupKey]; ok {
			if !reflect.DeepEqual(prev, e) {
				t.Errorf("harnesstest: DedupKey %q was parsed twice with different fields\n first:  %+v\n second: %+v", e.DedupKey, prev, e)
			}
			continue
		}
		m.events[e.DedupKey] = e
	}
	for _, s := range s.Sessions {
		m.sessions[s.SessionID] = mergeSession(m.sessions[s.SessionID], s)
	}
}

// mergeSession mirrors the store's session upsert (internal/store/store.go:113):
// a non-empty field from the newer batch wins, started_at is the minimum and
// updated_at the maximum, and empty values never erase what was known before.
// A resumable parser may legitimately report only what its latest batch knew.
func mergeSession(prev, next model.SessionMeta) model.SessionMeta {
	out := prev
	if next.SessionID != "" {
		out.SessionID = next.SessionID
	}
	if next.Harness != "" {
		out.Harness = next.Harness
	}
	if next.ParentID != "" {
		out.ParentID = next.ParentID
	}
	if next.Title != "" {
		out.Title = next.Title
	}
	if next.Project != "" {
		out.Project = next.Project
	}
	switch {
	case prev.StartedAt.IsZero():
		out.StartedAt = next.StartedAt
	case next.StartedAt.IsZero():
	default:
		if next.StartedAt.Before(prev.StartedAt) {
			out.StartedAt = next.StartedAt
		}
	}
	if next.UpdatedAt.After(prev.UpdatedAt) {
		out.UpdatedAt = next.UpdatedAt
	}
	return out
}

func (m *merger) compare(t testing.TB, want Snapshot) {
	t.Helper()
	gotEvents := make([]model.UsageEvent, 0, len(m.events))
	for _, e := range m.events {
		gotEvents = append(gotEvents, e)
	}
	sort.Slice(gotEvents, func(i, j int) bool { return gotEvents[i].DedupKey < gotEvents[j].DedupKey })
	wantEvents := dedupeEvents(t, want.Events)
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Errorf("harnesstest: incremental parsing does not match one full parse\n missing: %s\n extra:   %s",
			eventSummary(missingEvents(wantEvents, gotEvents)), eventSummary(missingEvents(gotEvents, wantEvents)))
	}
	gotSessions := make([]model.SessionMeta, 0, len(m.sessions))
	for _, s := range m.sessions {
		gotSessions = append(gotSessions, s)
	}
	sort.Slice(gotSessions, func(i, j int) bool { return gotSessions[i].SessionID < gotSessions[j].SessionID })
	if want := dedupeSessions(want.Sessions); !reflect.DeepEqual(gotSessions, want) {
		t.Errorf("harnesstest: incremental sessions do not match one full parse\n got:  %+v\n want: %+v", gotSessions, want)
	}
}

func dedupeEvents(t testing.TB, in []model.UsageEvent) []model.UsageEvent {
	t.Helper()
	seen := map[string]bool{}
	out := make([]model.UsageEvent, 0, len(in))
	for _, e := range in {
		if seen[e.DedupKey] {
			continue
		}
		seen[e.DedupKey] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DedupKey < out[j].DedupKey })
	return out
}

// dedupeSessions applies the store's merge rules to the sessions of one parse,
// in parse order, so that a parser emitting the same session twice (parent and
// child files, for example) is compared the way the store would store it.
func dedupeSessions(in []model.SessionMeta) []model.SessionMeta {
	byID := map[string]model.SessionMeta{}
	var order []string
	for _, s := range in {
		if _, ok := byID[s.SessionID]; !ok {
			order = append(order, s.SessionID)
		}
		byID[s.SessionID] = mergeSession(byID[s.SessionID], s)
	}
	out := make([]model.SessionMeta, 0, len(byID))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

func duplicateKeys(s Snapshot) []string {
	seen := map[string]bool{}
	var dups []string
	for _, e := range s.Events {
		if seen[e.DedupKey] {
			dups = append(dups, e.DedupKey)
			continue
		}
		seen[e.DedupKey] = true
	}
	sort.Strings(dups)
	return dups
}

func keyList(s Snapshot) []string {
	out := make([]string, 0, len(s.Events))
	for _, e := range s.Events {
		out = append(out, e.DedupKey)
	}
	return out
}

func missing(want, got []string) []string {
	have := map[string]bool{}
	for _, s := range got {
		have[s] = true
	}
	var out []string
	for _, s := range want {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}

func missingEvents(want, got []model.UsageEvent) []model.UsageEvent {
	have := map[string]bool{}
	for _, e := range got {
		have[e.DedupKey] = true
	}
	var out []model.UsageEvent
	for _, e := range want {
		if !have[e.DedupKey] {
			out = append(out, e)
		}
	}
	return out
}

func eventSummary(events []model.UsageEvent) string {
	if len(events) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for i, e := range events {
		if i == 8 {
			fmt.Fprintf(&b, "\n  … %d more", len(events)-i)
			break
		}
		fmt.Fprintf(&b, "\n  %s %s %s %s %+v", e.DedupKey, e.SessionID, e.Model, e.Timestamp.Format("2006-01-02T15:04:05Z07:00"), e.Tokens)
	}
	return b.String()
}

// fixtureFiles lists the fixture's regular files, relative and sorted, leaving
// out the golden file and the seed directory.
func fixtureFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && d.Name() == seedDir {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.Base(rel) == "golden.json" || strings.HasSuffix(rel, ".golden.json") {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// seedDir holds the inputs a Case.Grow closure needs (sqlite dumps, entry
// lists). The suite never copies it into a parsed root.
const seedDir = "seed"

func copyTree(src, dst, skip string) error {
	skipAbs, _ := filepath.Abs(skip)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == seedDir {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		abs, _ := filepath.Abs(path)
		if abs == skipAbs || filepath.Base(path) == "golden.json" || strings.HasSuffix(path, ".golden.json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
}

func treeContains(t testing.TB, root, needle string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(needle)) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("harnesstest: walk %s: %v", root, err)
	}
	return found
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

func contextAround(blob []byte, needle string) string {
	i := bytes.Index(blob, []byte(needle))
	if i < 0 {
		return ""
	}
	lo, hi := i-80, i+len(needle)+80
	if lo < 0 {
		lo = 0
	}
	if hi > len(blob) {
		hi = len(blob)
	}
	return string(blob[lo:hi])
}

// diffLines renders the first few differing lines of a golden mismatch.
func diffLines(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		fmt.Fprintf(&b, "  line %d:\n    want: %s\n    got:  %s\n", i+1, wl, gl)
		if shown++; shown == 6 {
			b.WriteString("  …\n")
			break
		}
	}
	if shown == 0 {
		b.WriteString("  (files differ only in trailing bytes)\n")
	}
	return b.String()
}
