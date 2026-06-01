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
}

type BakeConfig struct {
	Mode            string   `json:"mode,omitempty"`
	WaitAfterLoadMS int      `json:"waitAfterLoadMs,omitempty"`
	WaitIdleMS      int      `json:"waitIdleMs,omitempty"`
	WaitSelector    string   `json:"waitSelector,omitempty"`
	TimeoutMS       int      `json:"timeoutMs,omitempty"`
	Scripts         []string `json:"scripts,omitempty"`
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
	host := u.Host
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
	if cfg == nil || cfg.Bake == nil {
		return nil
	}
	js := &oms.JSBakingOptions{}
	if mode, ok := parseJSModeString(cfg.Bake.Mode); ok {
		js.Mode = mode
	}
	if cfg.Bake.WaitAfterLoadMS > 0 {
		js.WaitAfterLoadMS = cfg.Bake.WaitAfterLoadMS
	}
	if cfg.Bake.WaitIdleMS > 0 {
		js.WaitNetworkIdleMS = cfg.Bake.WaitIdleMS
	}
	if sel := strings.TrimSpace(cfg.Bake.WaitSelector); sel != "" {
		js.WaitSelector = sel
	}
	if cfg.Bake.TimeoutMS > 0 {
		js.TimeoutMS = cfg.Bake.TimeoutMS
	}
	if len(cfg.Bake.Scripts) > 0 {
		for _, sc := range cfg.Bake.Scripts {
			if trimmed := strings.TrimSpace(sc); trimmed != "" {
				js.Scripts = append(js.Scripts, trimmed)
			}
		}
	}
	if js.Mode == oms.JSExecutionModeAuto && js.WaitAfterLoadMS == 0 && js.WaitNetworkIdleMS == 0 && js.WaitSelector == "" && js.TimeoutMS == 0 && len(js.Scripts) == 0 {
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
