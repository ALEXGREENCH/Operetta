package proxy

import (
	"strings"

	"operetta/oms"
)

func (s *Server) shouldServeLocalBookmarks() bool {
	switch s.cfg.BookmarkMode {
	case BookmarkModeRemote:
		return false
	case BookmarkModeLocal:
		return true
	default:
		return len(s.cfg.Bookmarks) > 0
	}
}

func (s *Server) renderLocalBookmarks(authCode, authPrefix string, opts *oms.RenderOptions) *oms.Page {
	if len(s.cfg.Bookmarks) == 0 && s.cfg.BookmarkMode == BookmarkModeAuto {
		return nil
	}
	page := oms.NewPage()
	if opts != nil {
		page.SetTransport(opts.ClientVersion, opts.Compression)
	}
	page.AddString("1/http://opera-mini.ru/bndex.php")
	if authCode != "" {
		page.AddAuthcode(authCode)
	}
	if authPrefix != "" {
		page.AddAuthprefix(authPrefix)
	}
	page.AddStyle(oms.StyleDefault)
	page.AddPlus()
	page.AddText("Bookmarks")
	page.AddBreak()
	bookmarks := s.cfg.Bookmarks
	if len(bookmarks) == 0 {
		bookmarks = parseBookmarks(defaultBookmarksSpec)
	}
	for _, bm := range bookmarks {
		target := bm.URL
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			target = "http://" + target
		}
		page.AddLink("0/"+target, bm.Title)
	}
	page.Finalize()
	if nb, err := oms.NormalizeOMSWithStag(page.Data, 4); err == nil && nb != nil {
		page.Data = nb
	}
	return page
}
