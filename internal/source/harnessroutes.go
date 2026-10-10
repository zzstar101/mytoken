package source

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzstar101/mytoken/internal/model"
)

// The harnesses below name their providers in their own config files, and
// those names are what their logs (and so attribution) use: pi's
// ~/.pi/agent/models.json, OpenCode's opencode.json, DSH's cordis profile and
// Hermes' config.yaml. Reading the base URL next to each name is how a relay
// site learns which events are its own. A key found in the same place gives a
// full credential; a key kept elsewhere (an environment variable that is not
// set for this process) still gives a route: Credential with an empty KeyID,
// which never becomes a site of its own but tells every site at that origin
// that this provider name sends traffic to it.

// configRoutes reads every harness config this package knows besides Claude
// Code and Codex. Missing files are skipped.
func (h *HarnessConfig) configRoutes() []Credential {
	var out []Credential
	out = append(out, h.piRoutes()...)
	out = append(out, h.openCodeRoutes()...)
	out = append(out, h.dshRoutes()...)
	out = append(out, h.hermesRoutes()...)
	return out
}

// route builds one credential or route, or reports false for an unusable URL
// or cc-switch's local proxy.
func route(h model.Harness, provider, base, key string) (Credential, bool) {
	origin, ok := Origin(base)
	if !ok || origin == LocalProxyOrigin || provider == "" {
		return Credential{}, false
	}
	return Credential{
		Origin:   origin,
		KeyID:    KeyID(key),
		Provider: provider,
		Harness:  h,
		Source:   "harness-config",
		Secret:   NewSecret(key),
	}, true
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// resolveKey expands the ways configs point at a key instead of holding it:
// "{env:NAME}" (OpenCode), "$NAME" / "${NAME}", or a bare upper-case NAME
// (pi). Anything else is the key itself.
func resolveKey(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, "{env:") && strings.HasSuffix(v, "}"):
		return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(v, "{env:"), "}"))
	case strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}"):
		return os.Getenv(v[2 : len(v)-1])
	case strings.HasPrefix(v, "$") && envName.MatchString(v[1:]):
		return os.Getenv(v[1:])
	case envName.MatchString(v):
		return os.Getenv(v)
	}
	return v
}

func (h *HarnessConfig) piRoutes() []Credential {
	raw, err := os.ReadFile(filepath.Join(h.home, ".pi", "agent", "models.json"))
	if err != nil {
		return nil
	}
	var v struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []Credential
	for name, p := range v.Providers {
		if c, ok := route(model.Pi, name, p.BaseURL, resolveKey(p.APIKey)); ok {
			out = append(out, c)
		}
	}
	return out
}

func (h *HarnessConfig) openCodeRoutes() []Credential {
	var out []Credential
	for _, p := range []string{
		filepath.Join(h.home, ".config", "opencode", "opencode.json"),
		filepath.Join(h.home, ".config", "opencode", "opencode.jsonc"),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v struct {
			Provider map[string]struct {
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  string `json:"apiKey"`
				} `json:"options"`
			} `json:"provider"`
		}
		if json.Unmarshal(stripJSONComments(raw), &v) != nil {
			continue
		}
		for name, p := range v.Provider {
			if c, ok := route(model.OpenCode, name, p.Options.BaseURL, resolveKey(p.Options.APIKey)); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// stripJSONComments drops whole-line // comments, enough for opencode.jsonc.
func stripJSONComments(raw []byte) []byte {
	var b bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// dshRoutes reads the providers of DSH's pi-ai plugin from every profile:
//
//   - name: "@deepseek-ai/dsh-llm-pi-ai"
//     config:
//     providers:
//     nerv-base:
//     apiKeyEnv: NERV_BASE_API_KEY
//     baseURL: https://api.nerv-base.com/v1
func (h *HarnessConfig) dshRoutes() []Credential {
	files, _ := filepath.Glob(filepath.Join(h.home, ".dsh", "profiles", "*", "cordis*.yml"))
	var out []Credential
	seen := map[string]bool{}
	for _, f := range files {
		if strings.Contains(filepath.Base(f), ".bak") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for name, fields := range yamlChildren(raw, "providers", "dsh-llm-pi-ai") {
			base := fields["baseURL"]
			if base == "" {
				base = fields["baseUrl"]
			}
			key := fields["apiKey"]
			if env := fields["apiKeyEnv"]; key == "" && env != "" {
				key = os.Getenv(env)
			}
			c, ok := route(model.DSH, name, base, key)
			if !ok || seen[c.Origin+"\x00"+c.KeyID+"\x00"+name] {
				continue
			}
			seen[c.Origin+"\x00"+c.KeyID+"\x00"+name] = true
			out = append(out, c)
		}
	}
	return out
}

// hermesRoutes reads the model block of ~/.hermes/config.yaml:
//
//	model:
//	  provider: rn
//	  base_url: https://api.example.com/v1
//	  api_key: sk-…
func (h *HarnessConfig) hermesRoutes() []Credential {
	raw, err := os.ReadFile(filepath.Join(h.home, ".hermes", "config.yaml"))
	if err != nil {
		return nil
	}
	m := yamlBlock(raw, "model")
	if c, ok := route(model.Hermes, m["provider"], m["base_url"], resolveKey(m["api_key"])); ok {
		return []Credential{c}
	}
	return nil
}

// yamlLine is one non-blank, non-comment line: its indent, key and scalar.
type yamlLine struct {
	indent     int
	key, value string
}

func yamlLines(raw []byte) []yamlLine {
	var out []yamlLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(trimmed)
		if strings.HasPrefix(trimmed, "- ") {
			// a list item's first key sits two columns in
			trimmed = trimmed[2:]
			indent += 2
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			out = append(out, yamlLine{indent: indent, value: trimmed})
			continue
		}
		out = append(out, yamlLine{indent: indent, key: strings.TrimSpace(k), value: yamlScalar(v)})
	}
	return out
}

// yamlScalar unquotes a plain or quoted scalar and drops a trailing comment.
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// yamlBlock returns the scalar children of a top-level mapping key.
func yamlBlock(raw []byte, key string) map[string]string {
	out := map[string]string{}
	lines := yamlLines(raw)
	for i, l := range lines {
		if l.indent != 0 || l.key != key || l.value != "" {
			continue
		}
		child := -1
		for _, c := range lines[i+1:] {
			if c.indent == 0 {
				break
			}
			if child < 0 {
				child = c.indent
			}
			if c.indent == child && c.value != "" {
				out[c.key] = c.value
			}
		}
		break
	}
	return out
}

// yamlChildren finds a mapping named key that follows a line mentioning
// within (the plugin entry it belongs to), and returns each of its children
// with that child's scalar fields at any depth.
func yamlChildren(raw []byte, key, within string) map[string]map[string]string {
	out := map[string]map[string]string{}
	lines := yamlLines(raw)
	in := false
	for i, l := range lines {
		if strings.Contains(l.value, within) || strings.Contains(l.key, within) {
			in = true
			continue
		}
		if !in || l.key != key || l.value != "" {
			continue
		}
		child := -1
		var name string
		for _, c := range lines[i+1:] {
			if c.indent <= l.indent {
				break
			}
			if child < 0 {
				child = c.indent
			}
			if c.indent == child {
				name = c.key
				if out[name] == nil {
					out[name] = map[string]string{}
				}
				continue
			}
			if name != "" && c.value != "" {
				if _, dup := out[name][c.key]; !dup {
					out[name][c.key] = c.value
				}
			}
		}
		in = false
	}
	return out
}

// hostName names a gateway by its host the way attribution does for a
// generic provider ("api.example.com", "10.0.0.1:3000").
func hostName(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return u.Host
}
