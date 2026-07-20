package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"golang.org/x/net/html"
	"operetta/oms"
)

func main() {
	url := "https://sefan.ru/"
	showOperations := false
	for _, arg := range os.Args[1:] {
		if arg == "-ops" {
			showOperations = true
			continue
		}
		url = arg
	}
	if showOperations {
		dumpOperations(context.Background(), url)
		return
	}
	log.Printf("fetch %s", url)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "Opera/9.80 (J2ME/MIDP; Opera Mini/4.2.15410/37.8069; U; en) Presto/2.12.423 Version/12.16")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	doc, err := html.Parse(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	base := url
	ss := oms.BuildStylesheetForDebug(doc, base, resp.Header, nil)
	if ss == nil {
		log.Fatal("no stylesheet")
	}
	anchors := 0
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode {
			cls := strings.Fields(oms.GetAttr(n, "class"))
			if len(cls) > 0 {
				hasBalls := false
				for _, c := range cls {
					if c == "balls" {
						hasBalls = true
						break
					}
				}
				if hasBalls {
					props := oms.ComputeStyleForDebug(n, ss)
					fmt.Printf("node=%s classes=%v props=%v\n", n.Data, cls, props)
				}
			}
			if strings.EqualFold(n.Data, "a") && anchors < 30 {
				anchors++
				fmt.Printf("anchor href=%q class=%q props=%v\n", oms.GetAttr(n, "href"), oms.GetAttr(n, "class"), oms.ComputeStyleForDebug(n, ss))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)
}

func dumpOperations(ctx context.Context, target string) {
	headers := http.Header{
		"Accept-Language": []string{"ru,en;q=0.8"},
		"User-Agent":      []string{"Opera/9.80 (J2ME/MIDP; Opera Mini/4.2.15410/37.8069; U; en) Presto/2.12.423 Version/12.16"},
	}
	opts := &oms.RenderOptions{ImagesOn: true, ImageMIME: "image/jpeg", MaxInlineKB: 48, ScreenW: 231, ScreenH: 320, ReqHeaders: headers}
	document, err := oms.FetchDocumentWithHeadersAndOptionsCtx(ctx, target, headers, opts)
	if err != nil {
		log.Fatal(err)
	}
	model, err := oms.TransformDocument(document, headers, opts)
	if err != nil {
		log.Fatal(err)
	}
	for index, op := range model.Operations {
		fmt.Printf("%03d kind=%02d text=%q url=%q size=%dx%d data=%d color=%q style=%+v\n",
			index, op.Kind, op.Text, op.URL, op.Width, op.Height, len(op.Data), op.Color, op.Style)
	}
}
