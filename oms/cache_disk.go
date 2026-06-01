package oms

import (
	"crypto/sha1"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	diskCacheOnce sync.Once
	diskCacheDir  string
	diskCacheMax  int64
	diskCacheMu   sync.Mutex
)

func initDiskCache() {
	diskCacheDir = os.Getenv("OMS_IMG_CACHE_DIR")
	if diskCacheDir == "" {
		diskCacheDir = filepath.Join("cache", "img")
	}
	if err := os.MkdirAll(diskCacheDir, 0o755); err != nil {
		return
	}
	mb := 100
	if s := os.Getenv("OMS_IMG_CACHE_MB"); s != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 0 {
			mb = v
		}
	}
	diskCacheMax = int64(mb) * 1024 * 1024
}

func diskKey(format string, quality int, url string) (string, string) {
	h := sha1.Sum([]byte(format + "|q=" + strconv.Itoa(quality) + "|" + url))
	hex := make([]byte, 40)
	const hexd = "0123456789abcdef"
	for i, b := range h[:] {
		hex[i*2] = hexd[b>>4]
		hex[i*2+1] = hexd[b&0xF]
	}
	dir := filepath.Join(diskCacheDir, string(hex[0]), string(hex[1]))
	return dir, filepath.Join(dir, string(hex)+".bin")
}

func diskCacheGet(format string, quality int, url string) ([]byte, int, int, bool) {
	diskCacheOnce.Do(initDiskCache)
	dir, path := diskKey(format, quality, url)
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, false
	}
	defer f.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, 0, 0, false
	}
	w := int(binary.BigEndian.Uint16(header[0:2]))
	h := int(binary.BigEndian.Uint16(header[2:4]))
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, 0, false
	}
	_ = os.Chtimes(path, time.Now(), time.Now())
	_ = os.MkdirAll(dir, 0o755)
	return b, w, h, true
}

func diskCachePut(format string, quality int, url string, data []byte, w, h int) {
	diskCacheOnce.Do(initDiskCache)
	dir, path := diskKey(format, quality, url)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	var hdr [4]byte
	binary.BigEndian.PutUint16(hdr[0:2], uint16(w))
	binary.BigEndian.PutUint16(hdr[2:4], uint16(h))
	_, _ = f.Write(hdr[:])
	_, _ = f.Write(data)
	_ = f.Close()
	_ = os.Rename(tmp, path)
	go pruneDiskCache()
}

func pruneDiskCache() {
	diskCacheMu.Lock()
	defer diskCacheMu.Unlock()
	var files []struct {
		p  string
		sz int64
		mt time.Time
	}
	var total int64
	filepath.WalkDir(diskCacheDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(p), ".bin") {
			return nil
		}
		if info, e := d.Info(); e == nil {
			files = append(files, struct {
				p  string
				sz int64
				mt time.Time
			}{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if total <= diskCacheMax || diskCacheMax <= 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mt.Before(files[j].mt) })
	for _, f := range files {
		if total <= diskCacheMax {
			break
		}
		_ = os.Remove(f.p)
		total -= f.sz
	}
}
