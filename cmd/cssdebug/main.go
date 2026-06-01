package main

import (
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
    if len(os.Args) > 1 {
        url = os.Args[1]
    }
    log.Printf("fetch %s", url)
    req, _ := http.NewRequest(http.MethodGet, url, nil)
    req.Header.Set("User-Agent", "cssdebug/1.0")
    req.Header.Set("Accept", "text/html,application/xhtml+xml")
    resp, err := http.DefaultClient.Do(req)
    if err != nil { log.Fatal(err) }
    defer resp.Body.Close()
    doc, err := html.Parse(resp.Body)
    if err != nil { log.Fatal(err) }
    base := url
    ss := oms.BuildStylesheetForDebug(doc, base, resp.Header, nil)
    if ss == nil { log.Fatal("no stylesheet") }
    var visit func(*html.Node)
    visit = func(n *html.Node) {
        if n.Type == html.ElementNode {
            cls := strings.Fields(oms.GetAttr(n, "class"))
            if len(cls) > 0 {
                hasBalls := false
                for _, c := range cls { if c == "balls" { hasBalls = true; break } }
                if hasBalls {
                    props := oms.ComputeStyleForDebug(n, ss)
                    fmt.Printf("node=%s classes=%v props=%v\n", n.Data, cls, props)
                }
            }
        }
        for c := n.FirstChild; c != nil; c = c.NextSibling { visit(c) }
    }
    visit(doc)
}

