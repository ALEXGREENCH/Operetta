Operetta — Opera Mini 2.x-compatible gateway
============================================

Operetta is a from-scratch Go reimplementation of the legacy Opera Mini 1.x/2.x/3.x
gateway (the component that fetched the web, rewrote it into OBML/OMS and streamed
it back to the handset). It is primarily tested against the popular 2.06 DG-SC mod,
but behaves like the historic proxy for most classic clients.

Highlights
----------
- Fully self-contained Go 1.24 codebase – no external C dependencies or JNI shims.
- `internal/proxy` provides a modular `Server` with injectable configuration,
  request logging, per-client cookie jars, pagination cache and site overrides.
- `oms` renders HTML → OMS/OBML: DOM traversal, inline CSS heuristics, image pipeline,
  pagination helpers, tag normalisation and diagnostics.
- Extensive documentation (protocol captures, tag references, rendering notes) under
  `docs/`.

Quick start
-----------
```bash
go run ./cmd/operetta         # listens on :8081 by default

# or with custom settings
PORT=9000 OMS_BOOKMARKS_MODE=remote go run ./cmd/operetta -addr :9000
```

The bundled `/` HTML page is handy for manual testing; real devices talk to the proxy
via the Opera Mini POST handshake described in `docs/operetta-server-doc.md`.

Configuration at a glance
-------------------------
Operetta can be configured via environment variables or programmatically through
`proxy.Config`:

| Env var | Purpose |
|---------|---------|
| `PORT` | Overrides the listen port for `cmd/operetta` (falls back to `-addr`). |
| `OMS_BOOKMARKS_MODE` | `remote/pass/passthrough` keeps Opera’s portal; anything else serves the local list. |
| `OMS_BOOKMARKS` | Comma-separated `title|url` pairs for the local bookmark page. |
| `OMS_SITES_DIR` | Directory with per-host JSON overrides (`mode`, custom headers). Defaults to `config/sites`. |
| `OMS_IMG_CACHE_DIR` / `OMS_IMG_CACHE_MB` | On-disk image cache location and size. |
| `OMS_TAGCOUNT_MODE` / `OMS_TAGCOUNT_DELTA` | Tweaks for legacy OMS tag-count compatibility. |

Embedding example:

```go
cfg := proxy.DefaultConfig()
cfg.Bookmarks = []proxy.Bookmark{
    {Title: "Yandex", URL: "https://yandex.ru"},
    {Title: "Wiki", URL: "https://en.wikipedia.org"},
}
srv := proxy.New(cfg)
http.ListenAndServe(":8081", srv)
```

Repository layout
-----------------
- `cmd/operetta/` – CLI entry point wiring the proxy server into `net/http.Server`.
- `internal/proxy/` – HTTP handlers, configuration, site overrides, caches, logging.
- `oms/` – Rendering engine split into focused modules (`page.go`, `normalize.go`,
  `cache_disk.go`, etc.).
- `config/sites/` – Example host overrides (`example.com.json`).
- `docs/` – English/Russian guides, protocol notes and OBML references.
- `dist/`, `build.*`, `Makefile` – helper scripts and packaging templates.

Documentation
-------------
- [Operetta server documentation (EN)](docs/operetta-server-doc.md)
- [Документация на русском](docs/operetta-server-doc-ru.md)
- [OBML tag notes](docs/OBML.md)
- [Legacy OMS protocol walkthrough](docs/oms_protocol.md)

Contributing & testing
----------------------
- `go test ./...` exercises the renderer and utility packages.
- `golangci-lint run` (optional) keeps formatting and vetting consistent.
- Issues and pull requests are welcome – the TODO list in the docs tracks ideas and
  behavioural edge cases.
