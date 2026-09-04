package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"operetta/oms"
)

type cacheEntry struct {
	data    []byte
	created time.Time
	stats   oms.TrafficStats
}

type pageCache struct {
	mu    sync.RWMutex
	now   func() time.Time
	data  map[string]cacheEntry
	bytes int64
}

const (
	pageCacheTTL        = 10 * time.Minute
	pageCacheMaxEntries = 256
	pageCacheMaxBytes   = 64 << 20
)

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
	if opt == nil || opt.CachePartition == "" {
		return ""
	}
	js := ""
	if opt.JS != nil {
		js = fmt.Sprintf("%d/%d/%d/%d/%d/%s/%d/%t/%q/%q", opt.JS.Mode, opt.JS.WaitAfterLoadMS,
			opt.JS.WaitNetworkIdleMS, opt.JS.WaitDOMIdleMS, opt.JS.MaxSettleMS,
			opt.JS.WaitSelector, opt.JS.TimeoutMS, opt.JS.RasterizeEmoji, opt.JS.Scripts, opt.JS.FinalScripts)
	}
	effectiveTags, effectiveWireBytes, effectiveHeapBytes := oms.EffectivePaginationLimits(opt)
	variant := fmt.Sprintf(
		"target=%q|partition=%q|mime=%q|images=%t|hq=%t|maxkb=%d|maxpage=%d|pageheap=%d|screen=%dx%d|colors=%d|heap=%d|alpha=%d|version=%d|dialect=%q|basic=%t|compression=%d|tags=%d|lang=%q|ua=%q|js=%s",
		target, opt.CachePartition, opt.ImageMIME, opt.ImagesOn, opt.HighQuality,
		opt.MaxInlineKB, effectiveWireBytes, effectiveHeapBytes, opt.ScreenW, opt.ScreenH, opt.NumColors, opt.HeapBytes,
		opt.AlphaLevels, opt.ClientVersion, opt.DialectID, opt.LegacyBasicOM2, opt.Compression, effectiveTags,
		opt.ReqHeaders.Get("Accept-Language"), opt.ReqHeaders.Get("User-Agent"), js,
	)
	sum := sha256.Sum256([]byte(variant))
	return fmt.Sprintf("%x", sum[:])
}

func (c *pageCache) Store(target string, opt *oms.RenderOptions, hdr http.Header, page *oms.Page) {
	if opt == nil || page == nil || len(page.Data) == 0 || opt.Page > 1 {
		return
	}
	if page.NoCache || opt.FormBody != "" || hdr.Get("Authorization") != "" {
		return
	}
	data := page.Data
	if len(page.CachePacked) > 0 {
		data = page.CachePacked
	}
	key := cacheKey(target, opt)
	if key == "" || len(data) > pageCacheMaxBytes {
		return
	}
	entry := cacheEntry{data: append([]byte(nil), data...), created: c.now(), stats: page.Stats}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(entry.created)
	if previous, exists := c.data[key]; exists {
		c.bytes -= int64(len(previous.data))
	}
	c.data[key] = entry
	c.bytes += int64(len(entry.data))
	for len(c.data) > pageCacheMaxEntries || c.bytes > pageCacheMaxBytes {
		if !c.evictOldestLocked() {
			break
		}
	}
}

func (c *pageCache) Select(target string, opt *oms.RenderOptions) ([]byte, []string, int, int, oms.TrafficStats, bool) {
	if opt == nil || opt.Page <= 1 {
		return nil, nil, 0, 0, oms.TrafficStats{}, false
	}
	maxTags, maxWireBytes, maxHeapBytes := oms.EffectivePaginationLimits(opt)
	if maxTags <= 0 {
		return nil, nil, 0, 0, oms.TrafficStats{}, false
	}
	key := cacheKey(target, opt)
	if key == "" {
		return nil, nil, 0, 0, oms.TrafficStats{}, false
	}
	c.mu.Lock()
	entry, ok := c.data[key]
	if ok && c.now().Sub(entry.created) > pageCacheTTL {
		delete(c.data, key)
		c.bytes -= int64(len(entry.data))
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return nil, nil, 0, 0, oms.TrafficStats{}, false
	}
	var raw []byte
	var cur, cnt int
	var err error
	if opt.ServerBase != "" {
		raw, cur, cnt, err = oms.SelectOMSPartFromPackedWithNav(entry.data, opt.Page, maxTags, opt.ServerBase, target, opt)
	} else {
		raw, cur, cnt, err = oms.SelectOMSPartFromPackedWithBudgets(entry.data, opt.Page, maxTags, maxWireBytes, maxHeapBytes)
	}
	if err != nil {
		return nil, nil, 0, 0, oms.TrafficStats{}, false
	}
	return append([]byte(nil), raw...), nil, cur, cnt, entry.stats, true
}

func (c *pageCache) pruneLocked(now time.Time) {
	for key, entry := range c.data {
		if now.Sub(entry.created) <= pageCacheTTL {
			continue
		}
		delete(c.data, key)
		c.bytes -= int64(len(entry.data))
	}
}

func (c *pageCache) evictOldestLocked() bool {
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.data {
		if oldestKey == "" || entry.created.Before(oldest) {
			oldestKey = key
			oldest = entry.created
		}
	}
	if oldestKey == "" {
		return false
	}
	entry := c.data[oldestKey]
	delete(c.data, oldestKey)
	c.bytes -= int64(len(entry.data))
	return true
}
