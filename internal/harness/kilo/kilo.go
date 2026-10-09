// Package kilo parses Kilo Code (kilocode.kilo-code) task logs.
//
// # On-disk layout
//
// Kilo Code is a Roo Code fork and stores one directory per task under VS
// Code's globalStorage, in the same layout Roo Code uses:
//
//	~/Library/Application Support/<app>/User/globalStorage/kilocode.kilo-code/tasks/<taskId>/   (macOS)
//	~/.config/<app>/User/globalStorage/kilocode.kilo-code/tasks/<taskId>/                     (Linux)
//	%APPDATA%\<app>\User\globalStorage\kilocode.kilo-code\tasks\<taskId>\                     (Windows)
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
// Current Kilo Code revisions have moved to a new agent-manager store; the
// layout above is the legacy Roo-compatible store that packages/kilo-vscode/
// src/legacy-migration still reads (task-store.ts: API_FILE
// "api_conversation_history.json", UI_FILE "ui_messages.json", plus a sibling
// history_item.json per task directory). See package clinetask for the format
// details and the token accounting rules.
package kilo

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/harness/clinetask"
	"github.com/zzstar/mytoken/internal/model"
)

// Parser reads Kilo Code task logs. It holds no mutable state and is safe for
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
	if v := os.Getenv("MYTOKEN_KILO_DIRS"); v != "" {
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
	return clinetask.VSCodeTaskDirs(clinetask.ExtensionIDKilo)
}

func (p *Parser) Harness() model.Harness { return model.Kilo }

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

func (p *Parser) options() clinetask.Options {
	return clinetask.Options{
		Harness:            model.Kilo,
		ExtensionID:        clinetask.ExtensionIDKilo,
		InputIncludesCache: kiloInputIncludesCache,
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
// rewritten in place, so the cursor records the file fingerprint plus how many
// api_req_started entries were already emitted; a changed fingerprint means the
// whole file is re-read and only the entries past that counter are emitted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	return clinetask.Parse(src, cur, p.options())
}

// kiloInputIncludesCache always reports true: Kilo Code is a Roo Code fork and
// stores the TOTAL input tokens, cache tokens included, in tokensIn for both
// protocols — see Roo's packages/core/src/message-utils/
// consolidateTokenUsage.ts: "Since tokensIn now stores TOTAL input tokens
// (including cache tokens), we no longer need to add cacheWrites and cacheReads
// separately. This applies to both Anthropic and OpenAI protocols."
func kiloInputIncludesCache(string, string, string) bool { return true }
