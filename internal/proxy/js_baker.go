package proxy

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"operetta/oms"
)

type jsBaker struct {
	allocator context.Context
	cancel    context.CancelFunc
	logger    *log.Logger
}

func newJSBaker(logger *log.Logger) (*jsBaker, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("safebrowsing-disable-auto-update", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("disable-extensions", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	return &jsBaker{
		allocator: allocCtx,
		cancel:    cancel,
		logger:    logger,
	}, nil
}

func (b *jsBaker) Close() {
	if b.cancel != nil {
		b.cancel()
	}
}

func (b *jsBaker) Fetch(ctx context.Context, target string, hdr http.Header, opt *oms.RenderOptions, jsOpts *oms.JSBakingOptions) (*oms.UpstreamDocument, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("js fetch: empty target url")
	}
	// Derive context from allocator and bind to caller context for cancellation.
	taskCtx, cancelBrowser := chromedp.NewContext(b.allocator)
	defer cancelBrowser()

	if ctx != nil {
		var cancel context.CancelFunc
		taskCtx, cancel = context.WithCancel(taskCtx)
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-taskCtx.Done():
			}
		}()
		defer cancel()
	}

	timeout := 25 * time.Second
	if jsOpts != nil && jsOpts.TimeoutMS > 0 {
		timeout = time.Duration(jsOpts.TimeoutMS) * time.Millisecond
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		taskCtx, cancel = context.WithTimeout(taskCtx, timeout)
		defer cancel()
	}

	requestHeaders := cloneHeader(hdr)
	targetURL := target
	var finalURL string
	var htmlContent string
	var mainResp *network.Response
	var mainHeaders http.Header
	var encodedLen float64
	setCookieHeaders := []string{}

	var mu sync.Mutex
	activeRequests := 0
	lastActivity := time.Now()
	mainRequestID := network.RequestID("")

	chromedp.ListenTarget(taskCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			mu.Lock()
			activeRequests++
			lastActivity = time.Now()
			if e.Type == network.ResourceTypeDocument {
				mainRequestID = e.RequestID
			}
			mu.Unlock()
		case *network.EventLoadingFinished:
			mu.Lock()
			if activeRequests > 0 {
				activeRequests--
			}
			lastActivity = time.Now()
			if e.RequestID == mainRequestID {
				encodedLen = e.EncodedDataLength
			}
			mu.Unlock()
		case *network.EventLoadingFailed:
			mu.Lock()
			if activeRequests > 0 {
				activeRequests--
			}
			lastActivity = time.Now()
			mu.Unlock()
		case *network.EventResponseReceived:
			if e.RequestID == mainRequestID && e.Type == network.ResourceTypeDocument {
				mu.Lock()
				mainResp = e.Response
				mainHeaders = http.Header{}
				for k, v := range e.Response.Headers {
					switch hv := v.(type) {
					case string:
						mainHeaders.Add(k, hv)
					case []string:
						for _, item := range hv {
							mainHeaders.Add(k, item)
						}
					default:
						mainHeaders.Add(k, fmt.Sprint(hv))
					}
				}
				if mainResp != nil && mainResp.MimeType != "" && mainHeaders.Get("Content-Type") == "" {
					mainHeaders.Set("Content-Type", mainResp.MimeType)
				}
				if vals := mainHeaders.Values("Set-Cookie"); len(vals) > 0 {
					setCookieHeaders = append(setCookieHeaders, vals...)
				}
				mu.Unlock()
			}
		}
	})

	actions := []chromedp.Action{
		network.Enable(),
	}

	if ua := requestHeaders.Get("User-Agent"); ua != "" {
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			return emulation.SetUserAgentOverride(ua).Do(ctx)
		}))
		requestHeaders.Del("User-Agent")
	}
	if len(requestHeaders) > 0 {
		extra := network.Headers{}
		for k, vs := range requestHeaders {
			name := http.CanonicalHeaderKey(k)
			if strings.EqualFold(name, "Content-Length") {
				continue
			}
			if len(vs) == 0 {
				continue
			}
			extra[name] = strings.Join(vs, ", ")
		}
		if len(extra) > 0 {
			actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
				return network.SetExtraHTTPHeaders(extra).Do(ctx)
			}))
		}
	}

	if opt != nil && opt.Jar != nil {
		if u, err := url.Parse(targetURL); err == nil {
			if cookies := opt.Jar.Cookies(u); len(cookies) > 0 {
				params := make([]*network.CookieParam, 0, len(cookies))
				for _, c := range cookies {
					param := &network.CookieParam{
						Name:   c.Name,
						Value:  c.Value,
						Domain: cookieDomainForParam(c, u),
						Path:   cookiePathForParam(c),
						Secure: c.Secure,
					}
					if !c.Expires.IsZero() {
						exp := cdp.TimeSinceEpoch(c.Expires.UTC())
						param.Expires = &exp
					}
					param.HTTPOnly = c.HttpOnly
					params = append(params, param)
				}
				actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
					if len(params) == 0 {
						return nil
					}
					return network.SetCookies(params).Do(ctx)
				}))
			}
		}
	}

	actions = append(actions,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)

	if jsOpts != nil && strings.TrimSpace(jsOpts.WaitSelector) != "" {
		actions = append(actions, chromedp.WaitVisible(strings.TrimSpace(jsOpts.WaitSelector), chromedp.ByQuery))
	}

	if jsOpts != nil && jsOpts.WaitNetworkIdleMS > 0 {
		waitDur := time.Duration(jsOpts.WaitNetworkIdleMS) * time.Millisecond
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				mu.Lock()
				active := activeRequests
				elapsed := time.Since(lastActivity)
				mu.Unlock()
				if active == 0 && elapsed >= waitDur {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}))
	}

	if jsOpts != nil && jsOpts.WaitAfterLoadMS > 0 {
		actions = append(actions, chromedp.Sleep(time.Duration(jsOpts.WaitAfterLoadMS)*time.Millisecond))
	}

	if jsOpts != nil && len(jsOpts.Scripts) > 0 {
		for _, snippet := range jsOpts.Scripts {
			code := strings.TrimSpace(snippet)
			if code == "" {
				continue
			}
			actions = append(actions, chromedp.Evaluate(code, nil))
		}
	}

	actions = append(actions,
		chromedp.Location(&finalURL),
		chromedp.OuterHTML("html", &htmlContent, chromedp.ByQuery),
	)

	var browserCookies []*network.Cookie
	actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
		cmd := network.GetCookies()
		if finalURL != "" {
			cmd = cmd.WithURLs([]string{finalURL})
		} else if targetURL != "" {
			cmd = cmd.WithURLs([]string{targetURL})
		}
		var err error
		browserCookies, err = cmd.Do(ctx)
		return err
	}))

	if err := chromedp.Run(taskCtx, actions...); err != nil {
		return nil, err
	}

	if finalURL == "" {
		finalURL = targetURL
	}

	header := cloneHeader(mainHeaders)
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "text/html; charset=utf-8")
	}

	setCookieMap := map[string]struct{}{}
	for _, sc := range setCookieHeaders {
		if trimmed := strings.TrimSpace(sc); trimmed != "" {
			setCookieMap[trimmed] = struct{}{}
		}
	}

	httpCookies := make([]*http.Cookie, 0, len(browserCookies))
	for _, c := range browserCookies {
		if c == nil {
			continue
		}
		hc := cookieFromNetwork(c)
		if hc != nil {
			httpCookies = append(httpCookies, hc)
			if sc := hc.String(); sc != "" {
				setCookieMap[sc] = struct{}{}
			}
		}
	}

	if opt != nil && opt.Jar != nil && len(httpCookies) > 0 {
		if u, err := url.Parse(finalURL); err == nil {
			opt.Jar.SetCookies(u, httpCookies)
		}
	}

	setCookies := make([]string, 0, len(setCookieMap))
	for sc := range setCookieMap {
		setCookies = append(setCookies, sc)
	}
	sort.Strings(setCookies)

	transferBytes := len(htmlContent)
	if encodedLen > 0 {
		transferBytes = int(encodedLen)
	}

	doc := &oms.UpstreamDocument{
		URL:           finalURL,
		Body:          []byte(htmlContent),
		RawBody:       []byte(htmlContent),
		TransferBytes: transferBytes,
		Header:        header,
		ContentLength: int64(len(htmlContent)),
		SetCookies:    setCookies,
	}
	if mainResp != nil {
		doc.Status = int(mainResp.Status)
	}
	return doc, nil
}

func cookieFromNetwork(c *network.Cookie) *http.Cookie {
	if c == nil {
		return nil
	}
	hc := &http.Cookie{
		Name:     c.Name,
		Value:    c.Value,
		Path:     c.Path,
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HTTPOnly,
	}
	if !c.Session && c.Expires > 0 {
		sec, frac := math.Modf(c.Expires)
		hc.Expires = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	switch c.SameSite {
	case network.CookieSameSiteLax:
		hc.SameSite = http.SameSiteLaxMode
	case network.CookieSameSiteStrict:
		hc.SameSite = http.SameSiteStrictMode
	case network.CookieSameSiteNone:
		hc.SameSite = http.SameSiteNoneMode
	}
	return hc
}

func cookieDomainForParam(c *http.Cookie, u *url.URL) string {
	if c.Domain != "" {
		return c.Domain
	}
	if u != nil {
		return u.Hostname()
	}
	return ""
}

func cookiePathForParam(c *http.Cookie) string {
	if c.Path != "" {
		return c.Path
	}
	return "/"
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}
