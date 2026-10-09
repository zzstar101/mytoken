// Package roo parses Roo Code (rooveterinaryinc.roo-cline) task logs.
//
// # On-disk layout
//
// Roo Code is a Cline fork and stores one directory per task under VS Code's
// globalStorage:
//
//	~/Library/Application Support/<app>/User/globalStorage/rooveterinaryinc.roo-cline/tasks/<taskId>/   (macOS)
//	~/.config/<app>/User/globalStorage/rooveterinaryinc.roo-cline/tasks/<taskId>/                     (Linux)
//	%APPDATA%\<app>\User\globalStorage\rooveterinaryinc.roo-cline\tasks\<taskId>\                     (Windows)
//
// for every app in the VS Code family (Code, Code - Insiders, Cursor,
// Windsurf, VSCodium, Trae).
//
// The task directory holds:
//
//	ui_messages.json              JSON array of ClineMessage — the usage log
//	api_conversation_history.json  JSON array of the API conversation
//	task_metadata.json             context + model_usage bookkeeping
//	history_item.json              {id, rootTaskId?, parentTaskId?, ts, task,
//	                                workspace, mode, …} — the subtask link
//
// See package clinetask for the format details and the token accounting rules.
package roo

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/harness/clinetask"
	"github.com/zzstar/mytoken/internal/model"
)

// Parser reads Roo Code task logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct{ roots []string }

// New returns the parser for the default roots: every VS Code family
// globalStorage tasks directory.
func New() *Parser { return NewWithRoots() }

// NewWithRoot returns a parser rooted at root. It exists for tests and for
// tools that keep logs outside the home directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. An empty list falls back
// to the default roots. Each root is either a globalStorage directory or a
// tasks directory; Discover finds the task files wherever they sit underneath
// it.
func NewWithRoots(roots ...string) *Parser {
	cp := make([]string, 0, len(roots))
	for _, r := range roots {
		if r = strings.TrimSpace(r); r != "" {
			cp = append(cp, filepath.Clean(r))
		}
	}
	if len(cp) == 0 {
		cp = defaultRoots()
	}
	return &Parser{roots: cp}
}

func defaultRoots() []string {
	if v := os.Getenv("MYTOKEN_ROO_DIRS"); v != "" {
		var out []string
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, filepath.Clean(p))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return clinetask.VSCodeTaskDirs(clinetask.ExtensionIDRoo)
}

func (p *Parser) Harness() model.Harness { return model.Roo }

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

func (p *Parser) options() clinetask.Options {
	return clinetask.Options{
		Harness:            model.Roo,
		ExtensionID:        clinetask.ExtensionIDRoo,
		InputIncludesCache: rooInputIncludesCache,
	}
}

// Discover walks every root and returns one Source per task file, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	opts := p.options()
	var out []harness.Source
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, clinetask.Discover(root, opts)...)
	}
	return out, nil
}

// Parse reads one task file incrementally. Task files are JSON documents
// rewritten in place (Roo writes them through safeWriteJson: lockfile + temp
// file + rename), so the cursor records the file fingerprint plus how many
// api_req_started entries were already emitted; a changed fingerprint means the
// whole file is re-read and only the entries past that counter are emitted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	return clinetask.Parse(src, cur, p.options())
}

// rooInputIncludesCache always reports true: Roo Code stores the TOTAL input
// tokens, cache tokens included, in tokensIn for both protocols — see
// packages/core/src/message-utils/consolidateTokenUsage.ts: "Since tokensIn now
// stores TOTAL input tokens (including cache tokens), we no longer need to add
// cacheWrites and cacheReads separately. This applies to both Anthropic and
// OpenAI protocols."
func rooInputIncludesCache(string, string, string) bool { return true }
