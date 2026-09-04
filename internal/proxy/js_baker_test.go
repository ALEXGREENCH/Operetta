package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"

	"operetta/oms"
)

func TestMakeJSSettlePlanDefaultsAndBounds(t *testing.T) {
	defaults := makeJSSettlePlan(nil)
	if defaults.minimum != 1500*time.Millisecond ||
		defaults.networkIdle != 350*time.Millisecond ||
		defaults.domIdle != 350*time.Millisecond ||
		defaults.maximum != 2500*time.Millisecond {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	custom := makeJSSettlePlan(&oms.JSBakingOptions{
		WaitAfterLoadMS:   2200,
		WaitNetworkIdleMS: 500,
		WaitDOMIdleMS:     600,
		MaxSettleMS:       1000,
	})
	if custom.minimum != 2200*time.Millisecond || custom.maximum != custom.minimum ||
		custom.networkIdle != 500*time.Millisecond || custom.domIdle != 600*time.Millisecond {
		t.Fatalf("unexpected bounded custom plan: %+v", custom)
	}
}

func TestTrackForPageSettleIgnoresLongLivedResources(t *testing.T) {
	if trackForPageSettle(network.ResourceTypeEventSource) || trackForPageSettle(network.ResourceTypeMedia) {
		t.Fatal("long-lived event/media requests must not block page capture")
	}
	for _, resourceType := range []network.ResourceType{
		network.ResourceTypeDocument,
		network.ResourceTypeScript,
		network.ResourceTypeFetch,
		network.ResourceTypeXHR,
	} {
		if !trackForPageSettle(resourceType) {
			t.Fatalf("resource %s should participate in page settling", resourceType)
		}
	}
}

func TestApplyJSOptionsIncludesSettleAndEmojiControls(t *testing.T) {
	opts := &oms.RenderOptions{}
	applyJSOptionsFromQuery(opts, url.Values{
		"js":          []string{"on"},
		"js_wait":     []string{"1200"},
		"js_idle":     []string{"300"},
		"js_dom_idle": []string{"450"},
		"js_settle":   []string{"2400"},
		"js_emoji":    []string{"1"},
		"js_script":   []string{"window.prepared=true"},
		"js_final_script": []string{
			"document.querySelector('.late').remove()",
		},
	})
	if opts.JS == nil || opts.JS.Mode != oms.JSExecutionModeEnabled ||
		opts.JS.WaitAfterLoadMS != 1200 || opts.JS.WaitNetworkIdleMS != 300 ||
		opts.JS.WaitDOMIdleMS != 450 || opts.JS.MaxSettleMS != 2400 ||
		!opts.JS.RasterizeEmoji || len(opts.JS.Scripts) != 1 || len(opts.JS.FinalScripts) != 1 || !shouldUseJS(opts.JS) {
		t.Fatalf("unexpected JS options: %+v", opts.JS)
	}
}

func TestRewriteOnlySiteConfigEnablesJSBaking(t *testing.T) {
	jsOpts := (&SiteConfig{Rewrite: &RewriteConfig{
		RemoveSelectors: []string{".advert"},
	}}).JSOptions()
	if jsOpts == nil || len(jsOpts.Scripts) != 1 || len(jsOpts.FinalScripts) != 1 || !shouldUseJS(jsOpts) {
		t.Fatalf("rewrite must enable JS baking: %+v", jsOpts)
	}
	if !strings.Contains(jsOpts.Scripts[0], `".advert"`) {
		t.Fatalf("rewrite selector was not JSON-encoded into script: %s", jsOpts.Scripts[0])
	}
	if strings.Contains(jsOpts.FinalScripts[0], `"click":["`) || !strings.Contains(jsOpts.FinalScripts[0], `".advert"`) {
		t.Fatalf("final rewrite must clean without repeating clicks: %s", jsOpts.FinalScripts[0])
	}
}

func TestJSBakerWaitsForAsyncDOMAndRasterizesEmoji(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><body>`+
			`<aside class="noise">remove me</aside>`+
			`<main id="content"><div id="result">loading</div>`+
			`<button id="load" onclick="setTimeout(function(){document.getElementById('result').textContent='ready 😀';var late=document.createElement('aside');late.className='noise';late.textContent='late noise';document.body.appendChild(late);},180)">load</button>`+
			`</main></body></html>`)
	}))
	defer upstream.Close()

	baker, err := newJSBaker(log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newJSBaker: %v", err)
	}
	defer baker.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	jsOpts := (&SiteConfig{
		Bake: &BakeConfig{
			Mode:            "on",
			WaitAfterLoadMS: 350,
			WaitIdleMS:      50,
			WaitDOMIdleMS:   50,
			MaxSettleMS:     1000,
			TimeoutMS:       12000,
			EmojiAsImages:   true,
		},
		Rewrite: &RewriteConfig{
			MainSelector:    "#content",
			ClickSelectors:  []string{"#load"},
			RemoveSelectors: []string{"#load", ".noise"},
			CSS:             "#result{display:block}",
		},
	}).JSOptions()
	doc, err := baker.Fetch(ctx, upstream.URL, http.Header{}, &oms.RenderOptions{}, jsOpts)
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "executable file not found") || strings.Contains(lower, "could not find chrome") {
			t.Skipf("Chrome unavailable: %v", err)
		}
		t.Fatalf("Fetch: %v", err)
	}
	body := string(doc.Body)
	if !strings.Contains(body, `id="result">ready `) {
		t.Fatalf("late DOM mutation was not captured: %s", body)
	}
	if strings.Contains(body, "remove me") || strings.Contains(body, "late noise") || strings.Contains(body, `id="load"`) {
		t.Fatalf("site rewrite did not remove unwanted content: %s", body)
	}
	if !strings.Contains(body, `data-operetta-rewrite="1"`) {
		t.Fatalf("site rewrite CSS was not injected: %s", body)
	}
	if !strings.Contains(body, `<img`) || !strings.Contains(body, `data:image/png;base64,`) {
		t.Fatalf("emoji was not replaced with inline PNG: %s", body)
	}
	renderOpts := &oms.RenderOptions{
		ImagesOn:       true,
		ImageMIME:      "image/png",
		MaxInlineKB:    8,
		ScreenW:        240,
		ScreenH:        320,
		ClientVersion:  oms.ClientVersion2,
		LegacyBasicOM2: true,
	}
	page, err := oms.RenderDocument(doc, http.Header{}, renderOpts)
	if err != nil {
		t.Fatalf("RenderDocument: %v", err)
	}
	analysis := analyzeOMS(page.Data)
	if !analysis.ParseOK || analysis.Counts["I"] == 0 {
		t.Fatalf("rasterized emoji did not reach inline OMS image: parse=%v error=%q counts=%v", analysis.ParseOK, analysis.ParseError, analysis.Counts)
	}
	mustContain(t, analysis.Summary.Text, "ready")
}
