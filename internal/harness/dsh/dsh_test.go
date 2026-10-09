package dsh

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

// fixtureRoot points at testdata/dsh/sessions, a sanitized copy of a real
// ~/.dsh/sessions tree: two project slugs, three session directories, a v3/v4
// pair (v4 wins), a session.lock, a stray text file and a plain uncompressed
// log. Every message text is a PLACEHOLDER_* string.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "testdata", "dsh", "sessions")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture tree missing: %v", err)
	}
	return root
}

func parseAll(t *testing.T, p *Parser) ([]model.UsageEvent, []model.SessionMeta) {
	t.Helper()
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var events []model.UsageEvent
	var sessions []model.SessionMeta
	for _, src := range srcs {
		b, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		events = append(events, b.Events...)
		sessions = append(sessions, b.Sessions...)
	}
	return events, sessions
}

func sum(events []model.UsageEvent) model.Tokens {
	var t model.Tokens
	for _, e := range events {
		t = t.Add(e.Tokens)
	}
	return t
}

// sumDedup mirrors what the store does with repeated dedup keys: the last
// copy wins. The parser deliberately emits every copy it sees.
func sumDedup(events []model.UsageEvent) model.Tokens {
	last := map[string]model.Tokens{}
	order := make([]string, 0, len(events))
	for _, e := range events {
		if _, ok := last[e.DedupKey]; !ok {
			order = append(order, e.DedupKey)
		}
		last[e.DedupKey] = e.Tokens
	}
	var t model.Tokens
	for _, k := range order {
		t = t.Add(last[k])
	}
	return t
}

func sessionByID(sessions []model.SessionMeta, id string) (model.SessionMeta, bool) {
	for _, s := range sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return model.SessionMeta{}, false
}

// TestDiscover checks file selection: the highest version wins, locks and
// non-log files are ignored, and both compression kinds are reported.
func TestDiscover(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]string{
		filepath.Join("--home-user-proj-alpha--", "session-11111111-1111-1111-1111-111111111111", "session.v4.jsonl.zstd"): kindJSONLzs,
		filepath.Join("--home-user-proj-alpha--", "22222222-2222-2222-2222-222222222222", "session.v4.jsonl.zstd"):         kindJSONLzs,
		filepath.Join("--home-user-proj-beta--", "session-33333333-3333-3333-3333-333333333333", "session.jsonl"):          kindJSONL,
	}
	if len(srcs) != len(want) {
		var got []string
		for _, s := range srcs {
			got = append(got, s.Path)
		}
		t.Fatalf("Discover returned %d sources, want %d: %v", len(srcs), len(want), got)
	}
	for _, s := range srcs {
		rel, err := filepath.Rel(fixtureRoot(t), s.Path)
		if err != nil {
			t.Fatal(err)
		}
		kind, ok := want[rel]
		if !ok {
			t.Errorf("unexpected source %s", s.Path)
			continue
		}
		if s.Kind != kind {
			t.Errorf("%s: Kind = %q, want %q", rel, s.Kind, kind)
		}
		if _, err := os.Stat(s.Path); err != nil {
			t.Errorf("%s: not readable: %v", s.Path, err)
		}
	}
}

// TestParseTotals asserts the exact token totals of the fixture tree.
func TestParseTotals(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, sessions := parseAll(t, p)

	// 4 events in the parent session (one message id written twice) + 1 in the
	// subagent session + 2 in the plain session.
	if len(events) != 7 {
		t.Errorf("got %d events, want 7", len(events))
	}
	keys := map[string]int{}
	for _, e := range events {
		keys[e.DedupKey]++
	}
	if keys["msg-aaaa-0002"] != 2 {
		t.Errorf("streaming duplicate msg-aaaa-0002 emitted %d times, want 2", keys["msg-aaaa-0002"])
	}
	if len(keys) != 6 {
		t.Errorf("got %d distinct dedup keys, want 6: %v", len(keys), keys)
	}

	// The parser emits every copy of a streamed message, so the raw sum is
	// higher than the deduplicated total; both are asserted.
	raw := sum(events)
	wantRaw := model.Tokens{Input: 6750, Output: 1900, CacheRead: 1200, CacheWrite: 400, Reasoning: 135}
	if raw != wantRaw {
		t.Errorf("raw totals = %+v, want %+v", raw, wantRaw)
	}
	got := sumDedup(events)
	want := model.Tokens{Input: 5250, Output: 1600, CacheRead: 1100, CacheWrite: 200, Reasoning: 85}
	if got != want {
		t.Errorf("deduplicated totals = %+v, want %+v", got, want)
	}

	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}

	cases := []struct {
		id      string
		tokens  model.Tokens // raw, streaming duplicates included
		title   string
		project string
		parent  string
		started time.Time
		updated time.Time
	}{
		{
			id:      "session-11111111-1111-1111-1111-111111111111",
			tokens:  model.Tokens{Input: 4150, Output: 1350, CacheRead: 200, CacheWrite: 400, Reasoning: 110},
			title:   "PLACEHOLDER_TITLE_ALPHA",
			project: "/home/user/proj-alpha",
			started: time.UnixMilli(1790000000000).UTC(),
			updated: time.UnixMilli(1790000005000).UTC(),
		},
		{
			id:      "22222222-2222-2222-2222-222222222222",
			tokens:  model.Tokens{Input: 500, Output: 100},
			title:   "PLACEHOLDER_SUBAGENT_TASK", // no session/title record: first real user message
			project: "/home/user/proj-alpha",
			parent:  "session-11111111-1111-1111-1111-111111111111",
			started: time.UnixMilli(1790000006000).UTC(),
			updated: time.UnixMilli(1790000007200).UTC(),
		},
		{
			id:      "session-33333333-3333-3333-3333-333333333333",
			tokens:  model.Tokens{Input: 2100, Output: 450, CacheRead: 1000, Reasoning: 25},
			title:   "PLACEHOLDER_TITLE_BETA",
			project: "/home/user/proj-beta",
			started: time.UnixMilli(1790000100000).UTC(),
			updated: time.UnixMilli(1790000104000).UTC(),
		},
	}
	for _, c := range cases {
		s, ok := sessionByID(sessions, c.id)
		if !ok {
			t.Errorf("session %s missing", c.id)
			continue
		}
		if s.Harness != model.DSH {
			t.Errorf("%s: Harness = %q", c.id, s.Harness)
		}
		if s.ParentID != c.parent {
			t.Errorf("%s: ParentID = %q, want %q", c.id, s.ParentID, c.parent)
		}
		if s.Title != c.title {
			t.Errorf("%s: Title = %q, want %q", c.id, s.Title, c.title)
		}
		if s.Project != c.project {
			t.Errorf("%s: Project = %q, want %q", c.id, s.Project, c.project)
		}
		if !s.StartedAt.Equal(c.started) {
			t.Errorf("%s: StartedAt = %s, want %s", c.id, s.StartedAt, c.started)
		}
		if !s.UpdatedAt.Equal(c.updated) {
			t.Errorf("%s: UpdatedAt = %s, want %s", c.id, s.UpdatedAt, c.updated)
		}
		var st model.Tokens
		for _, e := range events {
			if e.SessionID == c.id {
				st = st.Add(e.Tokens)
			}
		}
		if st != c.tokens {
			t.Errorf("%s: raw tokens = %+v, want %+v", c.id, st, c.tokens)
		}
	}
}

// TestEventFields checks the per-event mapping of provider, model, project and
// the UTC timestamp.
func TestEventFields(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, _ := parseAll(t, p)

	byKey := map[string]model.UsageEvent{}
	for _, e := range events {
		byKey[e.DedupKey] = e // last wins, matching the store
	}
	ev, ok := byKey["msg-cccc-0002"]
	if !ok {
		t.Fatal("msg-cccc-0002 missing")
	}
	if ev.Provider != "rinnebeat" || ev.Model != "gpt-6-sol" {
		t.Errorf("provider/model = %q/%q, want rinnebeat/gpt-6-sol", ev.Provider, ev.Model)
	}
	if ev.ProjectPath != "/home/user/proj-beta" {
		t.Errorf("ProjectPath = %q", ev.ProjectPath)
	}
	if want := time.UnixMilli(1790000102000).UTC(); !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", ev.Timestamp, want)
	}
	if ev.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp is not UTC: %s", ev.Timestamp.Location())
	}
	if ev.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil (DSH logs carry no cost)", *ev.CostUSD)
	}
	if ev.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty", ev.BaseURL)
	}
	// The compaction request is counted as its own event.
	if c, ok := byKey["comp-0001"]; !ok {
		t.Error("compaction/summary event missing")
	} else if c.Provider != "nerv-base" || c.Model != "deepseek-flash" ||
		c.Tokens != (model.Tokens{Input: 50, Output: 500}) {
		t.Errorf("compaction event = %+v (%s/%s)", c.Tokens, c.Provider, c.Model)
	}
	// Records without usage are never counted.
	for _, e := range events {
		if strings.Contains(e.DedupKey, "attempt") || strings.Contains(e.DedupKey, "tool") {
			t.Errorf("non-usage record leaked into events: %+v", e)
		}
	}
}

// TestDedupLastWins checks that the final copy of a streamed message wins.
func TestDedupLastWins(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, _ := parseAll(t, p)
	var copies []model.UsageEvent
	for _, e := range events {
		if e.DedupKey == "msg-aaaa-0002" {
			copies = append(copies, e)
		}
	}
	if len(copies) != 2 {
		t.Fatalf("got %d copies of msg-aaaa-0002, want 2", len(copies))
	}
	first, last := copies[0], copies[1]
	if first.Tokens != (model.Tokens{Input: 1500, Output: 300, CacheRead: 100, CacheWrite: 200, Reasoning: 50}) {
		t.Errorf("first copy = %+v", first.Tokens)
	}
	if last.Tokens != (model.Tokens{Input: 1600, Output: 350, CacheRead: 100, CacheWrite: 200, Reasoning: 60}) {
		t.Errorf("last copy = %+v", last.Tokens)
	}
	if !last.Timestamp.After(first.Timestamp) {
		t.Errorf("last copy is not newer: %s vs %s", last.Timestamp, first.Timestamp)
	}
}

// padLine is a usage-free record long enough to push a log past the 4KB
// fingerprint window, so that appending to it leaves the fingerprint intact
// and the cursor must advance by offset alone.
func padLine(seq int) string {
	return fmt.Sprintf(`{"type":"tool/result","seq":%d,"time":%d,"data":{"id":"call_%d","output":%q}}`,
		seq, 1790000100000+int64(seq), seq, strings.Repeat("P", 1200))
}

// paddedLog rebuilds the plain beta fixture with padding between the records,
// returning the bytes and the offset just after the first assistant message.
func paddedLog(t *testing.T) ([]byte, int64) {
	t.Helper()
	plain := filepath.Join(fixtureRoot(t), "--home-user-proj-beta--",
		"session-33333333-3333-3333-3333-333333333333", "session.jsonl")
	raw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(raw) // header, user, assistant, assistant, title, tool/result
	var b strings.Builder
	write := func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	write(lines[0])
	write(lines[1])
	write(padLine(100))
	write(padLine(101))
	write(padLine(102))
	write(lines[2])
	cut := int64(b.Len()) // just after the first assistant message
	write(padLine(103))
	write(padLine(104))
	write(lines[3])
	write(lines[4])
	write(lines[5])
	out := []byte(b.String())
	if len(out) < 5000 {
		t.Fatalf("padded log is only %d bytes, want >= 5000 (past the 4KB fingerprint window)", len(out))
	}
	return out, cut
}

// TestIncrementalResume parses the plain log in two halves and requires the
// resumed totals to equal a single full parse.
func TestIncrementalResume(t *testing.T) {
	full, half := paddedLog(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, full[:half], 0o644); err != nil { // only the first half exists
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	first, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if first.Next.Offset != half {
		t.Errorf("Next.Offset = %d, want %d", first.Next.Offset, half)
	}
	if first.Next.Size != half {
		t.Errorf("Next.Size = %d, want %d", first.Next.Size, half)
	}
	if first.Next.Fingerprint == "" {
		t.Error("Next.Fingerprint is empty")
	}
	if len(first.Events) != 1 { // the header, the user message and the first assistant message
		t.Errorf("first half produced %d events, want 1", len(first.Events))
	}

	// The rest of the log arrives; resuming from the cursor must pick it up.
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := p.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatalf("resumed Parse: %v", err)
	}
	if second.Next.Offset != int64(len(full)) {
		t.Errorf("resumed Next.Offset = %d, want %d", second.Next.Offset, len(full))
	}
	combined := sum(append(append([]model.UsageEvent{}, first.Events...), second.Events...))

	whole, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("full Parse: %v", err)
	}
	if got := sum(whole.Events); got != combined {
		t.Errorf("resumed totals %+v != full totals %+v", combined, got)
	}
	if want := (model.Tokens{Input: 2100, Output: 450, CacheRead: 1000, Reasoning: 25}); combined != want {
		t.Errorf("totals = %+v, want %+v", combined, want)
	}
	// No event may be produced twice by the two halves.
	seen := map[string]int{}
	for _, e := range append(append([]model.UsageEvent{}, first.Events...), second.Events...) {
		seen[e.DedupKey]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("dedup key %s produced %d times across halves", k, n)
		}
	}
	// The session metadata is the same session in both halves.
	if first.Sessions[0].SessionID != whole.Sessions[0].SessionID {
		t.Errorf("session id changed between halves: %q vs %q",
			first.Sessions[0].SessionID, whole.Sessions[0].SessionID)
	}
}

// TestTruncatedTrailingLine checks that a partially written record is neither
// consumed nor counted, and is picked up once it is complete.
func TestTruncatedTrailingLine(t *testing.T) {
	full, cut := paddedLog(t)
	// Cut the record that follows the boundary in half: no trailing newline.
	next := bytes.IndexByte(full[cut:], '\n')
	if next < 0 {
		t.Fatal("no record after the boundary")
	}
	partial := full[:cut+int64(next/2)]

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, partial, 0o644); err != nil {
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	b, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if b.Next.Offset != cut {
		t.Errorf("Next.Offset = %d, want %d (end of the last complete line)", b.Next.Offset, cut)
	}
	if got := sum(b.Events); got != (model.Tokens{Input: 2000, Output: 400}) {
		t.Errorf("totals with a truncated tail = %+v, want {2000 400}", got)
	}
	if len(b.Events) != 1 {
		t.Errorf("got %d events, want 1 (the incomplete record must be ignored)", len(b.Events))
	}

	// Complete the line: the same cursor now yields the rest of the file.
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), src, b.Next)
	if err != nil {
		t.Fatalf("resumed Parse: %v", err)
	}
	if got := sum(b2.Events); got != (model.Tokens{Input: 100, Output: 50, CacheRead: 1000, Reasoning: 25}) {
		t.Errorf("resumed totals = %+v, want the second assistant message", got)
	}
	if b2.Next.Offset != int64(len(full)) {
		t.Errorf("Next.Offset = %d, want %d", b2.Next.Offset, len(full))
	}

	// Once the whole log is present the totals match a single full parse.
	if got := sum(append(append([]model.UsageEvent{}, b.Events...), b2.Events...)); got !=
		(model.Tokens{Input: 2100, Output: 450, CacheRead: 1000, Reasoning: 25}) {
		t.Errorf("cumulative totals = %+v", got)
	}
	// A third parse from the final cursor must add nothing.
	b3, err := p.Parse(context.Background(), src, b2.Next)
	if err != nil {
		t.Fatalf("final Parse: %v", err)
	}
	if len(b3.Events) != 0 {
		t.Errorf("parsing past the end produced %d events", len(b3.Events))
	}
}

// TestFingerprintChangeReparses checks that a rewritten file is reparsed from 0
// even when its size is unchanged.
func TestFingerprintChangeReparses(t *testing.T) {
	plain := filepath.Join(fixtureRoot(t), "--home-user-proj-beta--",
		"session-33333333-3333-3333-3333-333333333333", "session.jsonl")
	full, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	b1, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sum(b1.Events); got != (model.Tokens{Input: 2100, Output: 450, CacheRead: 1000, Reasoning: 25}) {
		t.Fatalf("totals = %+v", got)
	}

	// Same length, different bytes: only the fingerprint can detect this.
	rewritten := strings.Replace(string(full), `"inputTokens":2000`, `"inputTokens":9000`, 1)
	rewritten = strings.Replace(rewritten, `"totalTokens":2400`, `"totalTokens":9400`, 1)
	if len(rewritten) != len(full) {
		t.Fatalf("rewrite changed the length: %d vs %d", len(rewritten), len(full))
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), src, b1.Next)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Next.Offset != 0 && b2.Next.Offset != int64(len(rewritten)) {
		t.Errorf("Next.Offset = %d, want 0 (reparse from the start)", b2.Next.Offset)
	}
	if got := sum(b2.Events); got != (model.Tokens{Input: 9100, Output: 450, CacheRead: 1000, Reasoning: 25}) {
		t.Errorf("totals after rewrite = %+v, want the rewritten values", got)
	}
	if b2.Next.Fingerprint == b1.Next.Fingerprint {
		t.Error("fingerprint did not change")
	}
}

// TestZstdSkipAndIdempotence checks that an unchanged zstd log is skipped and
// that reparsing it yields identical totals.
func TestZstdSkipAndIdempotence(t *testing.T) {
	root := fixtureRoot(t)
	src := harness.Source{
		Path: filepath.Join(root, "--home-user-proj-alpha--",
			"session-11111111-1111-1111-1111-111111111111", "session.v4.jsonl.zstd"),
		Kind: kindJSONLzs,
	}
	p := NewWithRoots(root)
	b1, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if b1.Next.Offset != 0 {
		t.Errorf("zstd Next.Offset = %d, want 0 (offsets do not apply)", b1.Next.Offset)
	}
	if b1.Next.Size == 0 || b1.Next.Fingerprint == "" || b1.Next.ModTime.IsZero() {
		t.Errorf("incomplete cursor for an unchanged-file check: %+v", b1.Next)
	}

	b2, err := p.Parse(context.Background(), src, b1.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Events) != 0 || len(b2.Sessions) != 0 {
		t.Errorf("unchanged zstd log reparsed: %d events, %d sessions", len(b2.Events), len(b2.Sessions))
	}
	if b2.Next != b1.Next {
		t.Errorf("cursor changed on a skipped file: %+v vs %+v", b2.Next, b1.Next)
	}

	// Forcing a reparse (fresh cursor) must reproduce the same totals.
	b3, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if sum(b3.Events) != sum(b1.Events) {
		t.Errorf("zstd reparse totals differ: %+v vs %+v", sum(b3.Events), sum(b1.Events))
	}
}

// TestV4SupersetOfV3 guards the version choice: reading v3 as well would count
// the compaction request twice and miss nothing else, so the totals differ.
func TestV4SupersetOfV3(t *testing.T) {
	root := fixtureRoot(t)
	dir := filepath.Join(root, "--home-user-proj-alpha--", "session-11111111-1111-1111-1111-111111111111")
	v4 := harness.Source{Path: filepath.Join(dir, "session.v4.jsonl.zstd"), Kind: kindJSONLzs}
	v3 := harness.Source{Path: filepath.Join(dir, "session.v3.jsonl.zstd"), Kind: kindJSONLzs}
	p := NewWithRoots(root)
	b4, err := p.Parse(context.Background(), v4, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	b3, err := p.Parse(context.Background(), v3, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b4.Events) != 4 {
		t.Errorf("v4 produced %d events, want 4", len(b4.Events))
	}
	if len(b3.Events) != 3 {
		t.Errorf("v3 produced %d events, want 3 (it predates the compaction)", len(b3.Events))
	}
	if sum(b3.Events) == sum(b4.Events) {
		t.Error("v3 and v4 have identical totals; the version test is vacuous")
	}
}

// TestRegistration checks the package registers itself.
func TestRegistration(t *testing.T) {
	var found bool
	for _, p := range harness.All() {
		if p.Harness() == model.DSH {
			found = true
		}
	}
	if !found {
		t.Error("dsh parser is not registered")
	}
	p := New()
	if p.Harness() != model.DSH {
		t.Errorf("Harness() = %q, want %q", p.Harness(), model.DSH)
	}
	roots := p.Roots()
	if len(roots) != 1 || roots[0] == "" {
		t.Errorf("Roots() = %v", roots)
	}
	if got := NewWithRoot("/tmp/x").Roots(); len(got) != 1 || got[0] != "/tmp/x" {
		t.Errorf("NewWithRoot Roots() = %v", got)
	}
	if got := NewWithRoots("/a", "/b").Roots(); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("NewWithRoots Roots() = %v", got)
	}
}

// TestMissingRoot checks that a non-existent root is not an error.
func TestMissingRoot(t *testing.T) {
	p := NewWithRoot(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("got %d sources, want 0", len(srcs))
	}
}

// lineBoundary returns the offset just after the nth '\n'-terminated line.
func lineBoundary(data []byte, n int) int64 {
	var off int64
	for i := 0; i < len(data) && n > 0; i++ {
		if data[i] == '\n' {
			n--
		}
		off = int64(i + 1)
	}
	return off
}

func splitLines(data []byte) []string {
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func lastLine(data []byte) string {
	lines := splitLines(data)
	return lines[len(lines)-1]
}
