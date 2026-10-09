package harnesstest_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// fakeParser is a minimal offset-based JSONL parser. The suite is tested
// against it so that a broken check fails here (one package, no harness
// dependency) instead of showing up as ten confusing harness failures.
type fakeParser struct{ roots []string }

func newFake(root string) harness.Parser { return fakeParser{roots: []string{root}} }

func (p fakeParser) Harness() model.Harness { return model.Harness("fake") }
func (p fakeParser) Roots() []string        { return p.roots }

func (p fakeParser) Discover(context.Context) ([]harness.Source, error) {
	var out []harness.Source
	for _, root := range p.roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".jsonl") {
				out = append(out, harness.Source{Path: path, Kind: "jsonl"})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type fakeLine struct {
	TS         time.Time `json:"ts"`
	ID         string    `json:"id"`
	SessionID  string    `json:"sessionId"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	Prompt     string    `json:"prompt"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cacheRead"`
	CacheWrite int64     `json:"cacheWrite"`
	Reasoning  int64     `json:"reasoning"`
	Text       string    `json:"text"`
}

func (p fakeParser) Parse(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	info, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	size := info.Size()
	offset := cur.Offset
	if offset > size || (cur.Size != 0 && size < cur.Size) {
		offset = 0 // the file was rewritten
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return harness.Batch{}, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return harness.Batch{}, err
	}
	var batch harness.Batch
	var consumed int64
	sessions := map[string]*model.SessionMeta{}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break // keep the partial trailing line for the next read
		}
		line, rest := data[:i], data[i+1:]
		data = rest
		consumed += int64(i + 1)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rl fakeLine
		if err := json.Unmarshal(line, &rl); err != nil || rl.ID == "" {
			continue
		}
		at := rl.TS.UTC()
		batch.Events = append(batch.Events, model.UsageEvent{
			Harness:   p.Harness(),
			DedupKey:  rl.ID,
			SessionID: rl.SessionID,
			Timestamp: at,
			Model:     rl.Model,
			Provider:  rl.Provider,
			Tokens: model.Tokens{
				Input:      rl.Input,
				Output:     rl.Output,
				CacheRead:  rl.CacheRead,
				CacheWrite: rl.CacheWrite,
				Reasoning:  rl.Reasoning,
			},
		})
		s := sessions[rl.SessionID]
		if s == nil {
			s = &model.SessionMeta{Harness: p.Harness(), SessionID: rl.SessionID, StartedAt: at, UpdatedAt: at}
			sessions[rl.SessionID] = s
		}
		if at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.UpdatedAt) {
			s.UpdatedAt = at
		}
		if s.Title == "" && rl.Prompt != "" {
			s.Title = harness.Title(rl.Prompt)
		}
	}
	for _, s := range sessions {
		batch.Sessions = append(batch.Sessions, *s)
	}
	sort.Slice(batch.Sessions, func(i, j int) bool { return batch.Sessions[i].SessionID < batch.Sessions[j].SessionID })
	batch.Next = harness.Cursor{
		Offset:      offset + consumed,
		Size:        size,
		ModTime:     info.ModTime().UTC(),
		Fingerprint: fingerprint(src.Path),
	}
	return batch, nil
}

// fingerprint mirrors the real parsers: a hash of the file's first 4 KiB, used
// to detect a rewritten file whose size did not shrink.
func fingerprint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:])
}

func TestSuiteSelfCheck(t *testing.T) {
	harnesstest.Run(t, harnesstest.Case{
		Name:    "fake",
		New:     newFake,
		Fixture: "testdata/fake",
	})
}

func BenchmarkSuiteSelfCheck(b *testing.B) {
	harnesstest.Bench(b, harnesstest.Case{
		Name:    "fake",
		New:     newFake,
		Fixture: "testdata/fake",
	})
}

// docParser mimics the harnesses whose source is one JSON document rewritten in
// place (cline/roo/kilo) and whose project path is derived from the file's
// location (crush): the cursor therefore carries a fingerprint plus the number
// of entries already emitted, and every parse reports an absolute project path.
// It exercises Case.Grow together with Case.Rewrite.
type docParser struct{ roots []string }

func newDoc(root string) harness.Parser { return docParser{roots: []string{root}} }

func (p docParser) Harness() model.Harness { return model.Harness("fake-doc") }
func (p docParser) Roots() []string        { return p.roots }

func (p docParser) Discover(context.Context) ([]harness.Source, error) {
	var out []harness.Source
	for _, root := range p.roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !d.IsDir() && d.Name() == "task.json" {
				out = append(out, harness.Source{Path: path, Kind: "json"})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type docEntry struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"sessionId"`
	TS         time.Time `json:"ts"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	Prompt     string    `json:"prompt"`
	Text       string    `json:"text"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cacheRead"`
	CacheWrite int64     `json:"cacheWrite"`
	Reasoning  int64     `json:"reasoning"`
}

type docFile struct {
	Entries []docEntry `json:"entries"`
}

func (p docParser) Parse(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	var doc docFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return harness.Batch{}, err
	}
	info, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	fp := fingerprint(src.Path)
	emitted := 0
	if cur.Fingerprint == fp {
		if n, err := strconv.Atoi(strings.TrimPrefix(cur.Extra, "json:")); err == nil {
			emitted = n
		}
	}
	project := filepath.Dir(src.Path) // path-derived, like crush
	titles := map[string]string{}
	for _, e := range doc.Entries {
		if e.SessionID != "" && e.Prompt != "" {
			if _, ok := titles[e.SessionID]; !ok {
				titles[e.SessionID] = harness.Title(e.Prompt)
			}
		}
	}
	var batch harness.Batch
	sessions := map[string]*model.SessionMeta{}
	for i := emitted; i < len(doc.Entries); i++ {
		e := doc.Entries[i]
		if e.ID == "" {
			continue // a record without usage
		}
		at := e.TS.UTC()
		batch.Events = append(batch.Events, model.UsageEvent{
			Harness:     p.Harness(),
			DedupKey:    e.ID,
			SessionID:   e.SessionID,
			ProjectPath: project,
			Timestamp:   at,
			Model:       e.Model,
			Provider:    e.Provider,
			Tokens: model.Tokens{
				Input:      e.Input,
				Output:     e.Output,
				CacheRead:  e.CacheRead,
				CacheWrite: e.CacheWrite,
				Reasoning:  e.Reasoning,
			},
		})
		s := sessions[e.SessionID]
		if s == nil {
			s = &model.SessionMeta{
				Harness:   p.Harness(),
				SessionID: e.SessionID,
				Project:   project,
				Title:     titles[e.SessionID],
				StartedAt: at,
				UpdatedAt: at,
			}
			sessions[e.SessionID] = s
		}
		if at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.UpdatedAt) {
			s.UpdatedAt = at
		}
	}
	for _, s := range sessions {
		batch.Sessions = append(batch.Sessions, *s)
	}
	sort.Slice(batch.Sessions, func(i, j int) bool { return batch.Sessions[i].SessionID < batch.Sessions[j].SessionID })
	batch.Next = harness.Cursor{
		Offset:      info.Size(),
		Size:        info.Size(),
		ModTime:     info.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       "json:" + strconv.Itoa(len(doc.Entries)),
	}
	return batch, nil
}

const docSeed = "testdata/docseed/seed/entries.json"

// growDoc rewrites the task file with the first (step+1)*2 seed entries, which
// is how the suite grows fixtures that are not append-only logs.
func growDoc(_ testing.TB, dir string, step int) error {
	raw, err := os.ReadFile(docSeed)
	if err != nil {
		return err
	}
	var entries []docEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	n := (step + 1) * 2
	if n > len(entries) {
		n = len(entries)
	}
	out, err := json.MarshalIndent(docFile{Entries: entries[:n]}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tasks", "task.json"), out, 0o644)
}

func TestSuiteSelfCheckGrow(t *testing.T) {
	harnesstest.Run(t, harnesstest.Case{
		Name:    "fake-doc",
		New:     newDoc,
		Fixture: "testdata/docseed",
		Steps:   3,
		Grow:    growDoc,
		Rewrite: func(root string, snap *harnesstest.Snapshot) {
			for i := range snap.Events {
				snap.Events[i].ProjectPath = strings.Replace(snap.Events[i].ProjectPath, root, harnesstest.RootPlaceholder, 1)
			}
			for i := range snap.Sessions {
				snap.Sessions[i].Project = strings.Replace(snap.Sessions[i].Project, root, harnesstest.RootPlaceholder, 1)
			}
		},
	})
}
