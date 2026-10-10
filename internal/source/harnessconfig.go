package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/zzstar101/mytoken/internal/model"
)

// HarnessConfig is the set of local config files that say which provider each
// harness is pointing at right now: ~/.claude/settings.json (ANTHROPIC_BASE_URL)
// and ~/.codex/config.toml (model_provider / base_url). Only the endpoint is
// read; credentials in the same files are ignored.
type HarnessConfig struct {
	home string

	mu      sync.Mutex
	status  Status
	configs []CurrentConfig
}

// NewHarnessConfig returns the config files under home, which may not exist.
func NewHarnessConfig(home string) *HarnessConfig {
	return &HarnessConfig{home: home, status: Status{Name: "harness-config", Path: home}}
}

func (h *HarnessConfig) Name() string { return "harness-config" }

// Status returns the state of the last Load.
func (h *HarnessConfig) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Load re-reads both config files. Missing or unreadable files are skipped.
func (h *HarnessConfig) Load(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	configs := []CurrentConfig{}
	detail := []string{}
	if v, ok := h.readClaude(); ok {
		configs = append(configs, v)
		detail = append(detail, fmt.Sprintf("%s: %s", v.Harness, describe(v)))
	}
	if v, ok := h.readCodex(); ok {
		configs = append(configs, v)
		detail = append(detail, fmt.Sprintf("%s: %s", v.Harness, describe(v)))
	}
	_, err := os.Stat(h.home)
	h.configs = configs
	h.status.Exists = err == nil
	h.status.ReadAt = time.Now()
	h.status.Providers = len(configs)
	h.status.Detail = strings.Join(detail, ", ")
	if err != nil {
		h.status.Err = err.Error()
	}
	return ctx.Err()
}

func (h *HarnessConfig) readClaude() (CurrentConfig, bool) {
	raw, err := os.ReadFile(filepath.Join(h.home, ".claude", "settings.json"))
	if err != nil {
		return CurrentConfig{}, false
	}
	var v struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return CurrentConfig{}, false
	}
	out := CurrentConfig{Harness: model.ClaudeCode, Base: v.Env["ANTHROPIC_BASE_URL"]}
	if out.Base == "" {
		out.Provider = "anthropic"
	}
	return out, true
}

func (h *HarnessConfig) readCodex() (CurrentConfig, bool) {
	raw, err := os.ReadFile(filepath.Join(h.home, ".codex", "config.toml"))
	if err != nil {
		return CurrentConfig{}, false
	}
	var v struct {
		ModelProvider string `toml:"model_provider"`
		BaseURL       string `toml:"base_url"`
		Providers     map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if toml.Unmarshal(raw, &v) != nil {
		return CurrentConfig{}, false
	}
	base := v.BaseURL
	if p, ok := v.Providers[v.ModelProvider]; ok && p.BaseURL != "" {
		base = p.BaseURL
	}
	provider := v.ModelProvider
	if provider == "" {
		provider = "openai"
	}
	return CurrentConfig{Harness: model.Codex, Base: base, Provider: provider}, true
}

// describe renders a selection without any credential.
func describe(c CurrentConfig) string {
	if c.Base != "" {
		return c.Base
	}
	return c.Provider
}

// CurrentConfigs returns what each harness is pointing at, if anything.
func (h *HarnessConfig) CurrentConfigs() []CurrentConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]CurrentConfig{}, h.configs...)
}

// Close implements Source; the config files need no handles.
func (h *HarnessConfig) Close() error { return nil }

// Credentials enumerates the gateway credentials the harness config files
// point at. The key is read out of the same file as the base URL, used to
// derive the key id, and handed over inside a Secret; it is never returned as
// text by any other method and never written to disk by this package.
func (h *HarnessConfig) Credentials(ctx context.Context) ([]Credential, error) {
	_ = ctx
	var out []Credential
	if v, ok := h.readClaude(); ok && v.Base != "" {
		origin, ok := Origin(v.Base)
		if ok && origin != LocalProxyOrigin {
			out = append(out, Credential{
				Origin:   origin,
				KeyID:    KeyID(h.claudeKey()),
				Provider: hostName(origin),
				Harness:  v.Harness,
				Source:   "harness-config",
				Secret:   NewSecret(h.claudeKey()),
			})
		}
	}
	if v, ok := h.readCodex(); ok && v.Base != "" {
		origin, ok := Origin(v.Base)
		if ok && origin != LocalProxyOrigin {
			out = append(out, Credential{
				Origin:   origin,
				KeyID:    KeyID(h.codexKey()),
				Provider: providerName(v),
				Harness:  v.Harness,
				Source:   "harness-config",
				Secret:   NewSecret(h.codexKey()),
			})
		}
	}
	out = append(out, h.configRoutes()...)
	return out, nil
}

// claudeKey returns the token ~/.claude/settings.json sets for the harness, or
// "" when the harness talks to Anthropic directly.
func (h *HarnessConfig) claudeKey() string {
	raw, err := os.ReadFile(filepath.Join(h.home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var v struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Env["ANTHROPIC_AUTH_TOKEN"]
}

// codexKey returns the API key configured for the selected codex provider.
func (h *HarnessConfig) codexKey() string {
	raw, err := os.ReadFile(filepath.Join(h.home, ".codex", "config.toml"))
	if err != nil {
		return ""
	}
	var v struct {
		ModelProvider string `toml:"model_provider"`
		Providers     map[string]struct {
			EnvKey string `toml:"env_key"`
		} `toml:"model_providers"`
	}
	if toml.Unmarshal(raw, &v) != nil || v.ModelProvider == "" {
		return ""
	}
	envKey := v.Providers[v.ModelProvider].EnvKey
	if envKey == "" {
		return ""
	}
	return os.Getenv(envKey)
}

// providerName names a gateway the way the resolver would, so reconciliation
// reports can be joined against attributed events.
func providerName(c CurrentConfig) string {
	if c.Provider != "" {
		return c.Provider
	}
	return string(c.Harness)
}
