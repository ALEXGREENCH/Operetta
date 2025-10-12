package proxy

import (
	"net/url"
	"sync"

	"operetta/oms"
)

type renderPref struct {
	ImagesOn    bool
	HighQuality bool
	ImageMIME   string
	MaxInlineKB int
}

type renderPrefStore struct {
	mu   sync.RWMutex
	data map[string]renderPref
}

func newRenderPrefStore() *renderPrefStore {
	return &renderPrefStore{data: make(map[string]renderPref)}
}

func (s *renderPrefStore) Remember(key string, opt *oms.RenderOptions) {
	if opt == nil {
		return
	}
	s.mu.Lock()
	s.data[key] = renderPref{
		ImagesOn:    opt.ImagesOn,
		HighQuality: opt.HighQuality,
		ImageMIME:   opt.ImageMIME,
		MaxInlineKB: opt.MaxInlineKB,
	}
	s.mu.Unlock()
}

func (s *renderPrefStore) Apply(key string, opt *oms.RenderOptions, overrides url.Values) {
	if opt == nil {
		return
	}
	s.mu.RLock()
	pref, ok := s.data[key]
	s.mu.RUnlock()
	if !ok {
		return
	}
	if overrides.Get("img") == "" {
		opt.ImagesOn = pref.ImagesOn
	}
	if overrides.Get("hq") == "" {
		opt.HighQuality = pref.HighQuality
	}
	if overrides.Get("mime") == "" && pref.ImageMIME != "" {
		opt.ImageMIME = pref.ImageMIME
	}
	if overrides.Get("maxkb") == "" && pref.MaxInlineKB > 0 {
		opt.MaxInlineKB = pref.MaxInlineKB
	}
}
