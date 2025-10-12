package proxy

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type SiteConfig struct {
	Mode    string            `json:"mode"`
	Headers map[string]string `json:"headers,omitempty"`
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
