package proxy

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"operetta/oms"
)

type cacheEntry struct {
	data       []byte
	setCookies []string
	created    time.Time
}

type pageCache struct {
	mu   sync.RWMutex
	now  func() time.Time
	data map[string]cacheEntry
}

func newPageCache(now func() time.Time) *pageCache {
	if now == nil {
		now = time.Now
	}
	return &pageCache{
		now:  now,
		data: make(map[string]cacheEntry),
	}
}

func cacheKey(target string, opt *oms.RenderOptions) string {
	if opt == nil {
		return target
	}
	return target + "|" + opt.ImageMIME +
		":i=" + strconv.Itoa(boolToInt(opt.ImagesOn)) +
		":q=" + strconv.Itoa(boolToInt(opt.HighQuality)) +
		":w=" + strconv.Itoa(opt.ScreenW)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (c *pageCache) Store(target string, opt *oms.RenderOptions, _ http.Header, page *oms.Page) {
	if opt == nil || page == nil || len(page.Data) == 0 || opt.Page > 1 {
		return
	}
	if page.NoCache {
		return
	}
	data := page.Data
	if len(page.CachePacked) > 0 {
		data = page.CachePacked
	}
	entry := cacheEntry{
		data:       append([]byte(nil), data...),
		setCookies: append([]string(nil), page.SetCookies...),
		created:    c.now(),
	}
	key := cacheKey(target, opt)
	c.mu.Lock()
	c.data[key] = entry
	c.mu.Unlock()
}

func (c *pageCache) Select(target string, opt *oms.RenderOptions) ([]byte, []string, int, int, bool) {
	if opt == nil || opt.Page <= 1 || opt.MaxTagsPerPage <= 0 {
		return nil, nil, 0, 0, false
	}
	key := cacheKey(target, opt)
	c.mu.RLock()
	entry, ok := c.data[key]
	c.mu.RUnlock()
	if !ok {
		return nil, nil, 0, 0, false
	}
	var raw []byte
	var cur, cnt int
	var err error
	if opt.ServerBase != "" {
		raw, cur, cnt, err = oms.SelectOMSPartFromPackedWithNav(entry.data, opt.Page, opt.MaxTagsPerPage, opt.ServerBase, target, opt)
	} else {
		raw, cur, cnt, err = oms.SelectOMSPartFromPacked(entry.data, opt.Page, opt.MaxTagsPerPage)
	}
	if err != nil {
		return nil, nil, 0, 0, false
	}
	return append([]byte(nil), raw...), append([]string(nil), entry.setCookies...), cur, cnt, true
}
