package proxy

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"operetta/oms"
)

type SiteConfig struct {
	Mode    string            `json:"mode"`
	Headers map[string]string `json:"headers,omitempty"`
	Bake    *BakeConfig       `json:"bake,omitempty"`
	Rewrite *RewriteConfig    `json:"rewrite,omitempty"`
}

type BakeConfig struct {
	Mode            string   `json:"mode,omitempty"`
	WaitAfterLoadMS int      `json:"waitAfterLoadMs,omitempty"`
	WaitIdleMS      int      `json:"waitIdleMs,omitempty"`
	WaitDOMIdleMS   int      `json:"waitDomIdleMs,omitempty"`
	MaxSettleMS     int      `json:"maxSettleMs,omitempty"`
	WaitSelector    string   `json:"waitSelector,omitempty"`
	TimeoutMS       int      `json:"timeoutMs,omitempty"`
	EmojiAsImages   bool     `json:"emojiAsImages,omitempty"`
	Scripts         []string `json:"scripts,omitempty"`
}

// RewriteConfig is the declarative part of the legacy site template system.
// It deliberately exposes a small, auditable set of DOM operations; unusual
// sites can still use BakeConfig.Scripts as an escape hatch.
type RewriteConfig struct {
	MainSelector    string   `json:"mainSelector,omitempty"`
	ClickSelectors  []string `json:"clickSelectors,omitempty"`
	RemoveSelectors []string `json:"removeSelectors,omitempty"`
	UnwrapSelectors []string `json:"unwrapSelectors,omitempty"`
	CSS             string   `json:"css,omitempty"`
}

func (cfg *RewriteConfig) script() string {
	if cfg == nil {
		return ""
	}
	type payload struct {
		Main   string   `json:"main"`
		Click  []string `json:"click"`
		Remove []string `json:"remove"`
		Unwrap []string `json:"unwrap"`
		CSS    string   `json:"css"`
	}
	data, err := json.Marshal(payload{
		Main:   strings.TrimSpace(cfg.MainSelector),
		Click:  cfg.ClickSelectors,
		Remove: cfg.RemoveSelectors,
		Unwrap: cfg.UnwrapSelectors,
		CSS:    cfg.CSS,
	})
	if err != nil {
		return ""
	}
	return `(function(c){
  function each(list, fn) {
    (list || []).forEach(function(selector) {
      try { document.querySelectorAll(selector).forEach(fn); } catch (_) {}
    });
  }
  each(c.click, function(node) { if (typeof node.click === 'function') node.click(); });
  if (c.main) {
    try {
      var main = document.querySelector(c.main);
      if (main && main !== document.body) document.body.replaceChildren(main);
    } catch (_) {}
  }
  each(c.remove, function(node) { node.remove(); });
  each(c.unwrap, function(node) {
    if (node.parentNode) node.replaceWith.apply(node, Array.prototype.slice.call(node.childNodes));
  });
  if (c.css) {
    var style = document.createElement('style');
    style.setAttribute('data-operetta-rewrite', '1');
    style.textContent = c.css;
    (document.head || document.documentElement).appendChild(style);
  }
})(` + string(data) + `)`
}

type siteConfigStore struct {
	dir   string
	mu    sync.RWMutex
	cache map[string]*SiteConfig
}

func newSiteConfigStore(dir string) *siteConfigStore {
	return &siteConfigStore{
		dir:   dir,
		cache: make(map[string]*SiteConfig),
	}
}

func (s *siteConfigStore) Find(target string) *SiteConfig {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return nil
	}
	s.mu.RLock()
	if cfg, ok := s.cache[host]; ok {
		s.mu.RUnlock()
		return cfg
	}
	s.mu.RUnlock()

	labels := strings.Split(host, ".")
	for i := 0; i < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		if cfg := s.load(candidate); cfg != nil {
			s.mu.Lock()
			s.cache[host] = cfg
			s.mu.Unlock()
			return cfg
		}
	}
	s.mu.Lock()
	s.cache[host] = nil
	s.mu.Unlock()
	return nil
}

func (s *siteConfigStore) load(host string) *SiteConfig {
	if s.dir == "" {
		return nil
	}
	path := filepath.Join(s.dir, host+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg SiteConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	cfg.Mode = strings.TrimSpace(strings.ToLower(cfg.Mode))
	return &cfg
}

func (cfg *SiteConfig) JSOptions() *oms.JSBakingOptions {
	if cfg == nil || (cfg.Bake == nil && cfg.Rewrite == nil) {
		return nil
	}
	js := &oms.JSBakingOptions{}
	if cfg.Bake != nil {
		if mode, ok := parseJSModeString(cfg.Bake.Mode); ok {
			js.Mode = mode
		}
		if cfg.Bake.WaitAfterLoadMS > 0 {
			js.WaitAfterLoadMS = cfg.Bake.WaitAfterLoadMS
		}
		if cfg.Bake.WaitIdleMS > 0 {
			js.WaitNetworkIdleMS = cfg.Bake.WaitIdleMS
		}
		if cfg.Bake.WaitDOMIdleMS > 0 {
			js.WaitDOMIdleMS = cfg.Bake.WaitDOMIdleMS
		}
		if cfg.Bake.MaxSettleMS > 0 {
			js.MaxSettleMS = cfg.Bake.MaxSettleMS
		}
		if sel := strings.TrimSpace(cfg.Bake.WaitSelector); sel != "" {
			js.WaitSelector = sel
		}
		if cfg.Bake.TimeoutMS > 0 {
			js.TimeoutMS = cfg.Bake.TimeoutMS
		}
		js.RasterizeEmoji = cfg.Bake.EmojiAsImages
	}
	if rewrite := cfg.Rewrite.script(); rewrite != "" {
		js.Scripts = append(js.Scripts, rewrite)
	}
	if cfg.Bake != nil && len(cfg.Bake.Scripts) > 0 {
		for _, script := range cfg.Bake.Scripts {
			if trimmed := strings.TrimSpace(script); trimmed != "" {
				js.Scripts = append(js.Scripts, trimmed)
			}
		}
	}
	if js.Mode == oms.JSExecutionModeAuto && js.WaitAfterLoadMS == 0 && js.WaitNetworkIdleMS == 0 && js.WaitDOMIdleMS == 0 && js.MaxSettleMS == 0 && js.WaitSelector == "" && js.TimeoutMS == 0 && !js.RasterizeEmoji && len(js.Scripts) == 0 {
		return nil
	}
	return js
}

func parseJSModeString(raw string) (oms.JSExecutionMode, bool) {
	val := strings.TrimSpace(strings.ToLower(raw))
	switch val {
	case "", "auto", "default":
		return oms.JSExecutionModeAuto, true
	case "off", "disabled", "0", "false":
		return oms.JSExecutionModeDisabled, true
	case "on", "enabled", "1", "true":
		return oms.JSExecutionModeEnabled, true
	case "force", "required", "require":
		return oms.JSExecutionModeRequired, true
	}
	return oms.JSExecutionModeAuto, false
}
