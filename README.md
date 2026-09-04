Operetta — Opera Mini 2.x-compatible gateway
============================================

Operetta is a from-scratch Go reimplementation of the legacy Opera Mini 1.x/2.x/3.x
gateway (the component that fetched the web, rewrote it into OBML/OMS and streamed
it back to the handset). It is primarily tested against the popular 2.06 DG-SC mod,
but behaves like the historic proxy for most classic clients.

Highlights
----------
- Fully self-contained Go 1.25 codebase (Go 1.26.5 toolchain) – no external C dependencies or JNI shims.
- `internal/proxy` provides a modular `Server` with injectable configuration,
  request logging, per-client cookie jars, pagination cache and site overrides.
- `oms` renders HTML → OMS/OBML: DOM traversal, inline CSS heuristics, image pipeline,
  pagination helpers, tag normalisation and diagnostics.
- Extensive documentation (protocol captures, tag references, rendering notes) under
  `docs/`.

Quick start
-----------
```bash
go run ./cmd/operetta         # listens on 127.0.0.1:8081 by default

# or with custom settings
PORT=9000 OMS_BOOKMARKS_MODE=remote go run ./cmd/operetta -addr :9000
```

For protocol diagnostics, `-debug` prints parsed legacy parameters and
`-wire-debug` prints bounded hexadecimal request/response prefixes. Wire dumps
are disabled by default and never exceed 512 bytes per packet:

```powershell
go run ./cmd/operetta -debug -wire-debug
```

Opera Mini 4 reference mode
---------------------------

The experimental OM4 transport adapter can forward authenticated application
frames to a compatible reference server. This is useful for validating session
handling and building a request/response corpus while Operetta's native OM4
renderer is developed:

```powershell
go run ./cmd/operetta -debug `
  -om4-reference-url http://server4.operamini.com/ `
  -om4-corpus-dir build/om4-corpus
```

Reference mode is opt-in. The endpoint receives the browsing URLs and form data
sent by the MIDlet. The optional corpus contains decrypted frame payloads and
may include URLs, session identifiers, or page contents; keep it out of version
control. New captures store only a SHA-256 digest of the application header and
redact common password/token fields in form frames. Each corpus item has JSON metadata plus
`request.frames.bin` and `response.frames.bin`. Frame records use a one-byte
type, one-byte channel, four-byte big-endian payload length and the payload.

Sky Operetta can also request a bounded plaintext OM4 bridge by adding
`engine=4` and `backend=operetta|official` to its legacy NUL-KV POST. The
gateway returns `application/x-operetta-om4`: a genuine application document
in uncompressed MSS type `0x0a`, with the supported drawing subset and images
normalized to RGB565A. `backend=operetta` renders locally. The explicit
`backend=official` choice opens a fresh application session with
`http://server4.operamini.com/`; RSA/RC4/HMAC and raw-DEFLATE therefore stay
off the memory-constrained handset. A generic startup frame template is built
into the binary and contains no transport keys or application-session header.

Inspect a captured response and decode its page header/text instructions with:

```powershell
go run ./cmd/om4corpus -in build/om4-corpus/<capture>.response.frames.bin
```

The same command exports the decoded drawing stream as protocol-neutral
`sky.scene.v1`. The scene preserves absolute geometry, styles, normalized
links and embedded-image digests without copying image bytes into JSON:

```powershell
go run ./cmd/om4corpus `
  -in build/om4-corpus/<capture>.response.frames.bin `
  -scene-out build/om4-corpus/<capture>.scene.json
```

The anonymous comparison collector drives stateful reference and local OM4
sessions, fetches the same URLs through Operetta's protocol-neutral renderer and
writes semantic, native-page and scene JSON reports. Reference and local
exchanges for one case start together (with independent timeouts) to reduce
dynamic-site drift; cases stay sequential so each session's navigation-token
chain remains valid. A safe decrypted OM4 4.2 startup request is built in, so a
clean checkout does not need private historical captures. Explicit
`-bootstrap-request`, `-startup-request` and `-navigation-request` files remain
available for protocol research. Its default manifest contains world82, sefan,
OpenNet and twenty high-traffic global/Russian sites:

```powershell
go run ./cmd/om4compare `
  -sites config/om4-sites.txt `
  -operetta http://127.0.0.1:8081/ `
  -out build/om4-compare
```

The collector uses an Opera Mini 4/Presto User-Agent for the origin side because
several sites return materially different fallback HTML to legacy clients. It
stores separate `.reference.frames.bin` / `.operetta.frames.bin` pages and
matching `.reference.scene.json` / `.operetta.scene.json` files, then reports
missing/extra text and tokens, links, images, colors, geometry, byte-size,
document-height deltas and per-side duration. If the official endpoint is
temporarily unavailable, native collection continues; pass `-require-reference`
to retain fail-fast behavior. Scene links drop wire-only `0/`/`1/` prefixes and
leading NUL; relative targets are resolved through the document base URL while
query parameters remain intact. `report.html` is a self-contained visual
comparison: it renders both drawing streams with their actual embedded images,
synchronizes scrolling and can overlay every keyboard-focus rectangle.

`cmd/om4probe` can replay a complete captured OM4 wire request against a
reference endpoint for isolated protocol analysis. Corpus frame files are the
decrypted research format and are not wire-request inputs for that command.

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
| `OMS_SITES_DIR` | Directory with per-host JSON overrides (mode, headers, JS baking and legacy rewrites). Defaults to `config/sites`. |
| `OMS_IMG_CACHE_DIR` / `OMS_IMG_CACHE_MB` | On-disk image cache location and size. |
| `OMS_TAGCOUNT_MODE` / `OMS_TAGCOUNT_DELTA` | Tweaks for legacy OMS tag-count compatibility. |
| `OMS_ENABLE_DIAGNOSTICS` | `1` enables the network-capable `/validate` diagnostic endpoint; disabled by default. |
| `OMS_OM4_REFERENCE_URL` | Enables the experimental OM4 reference bridge to the specified HTTP(S) endpoint. |
| `OMS_OM4_OFFICIAL_URL` | Overrides the official endpoint used by explicit plaintext-bridge requests. |
| `OMS_OM4_STARTUP_REQUEST` | Optional encrypted `.bin` or decrypted `.frames.bin` startup template override. |
| `OMS_OM4_CORPUS_DIR` | Saves decrypted OM4 request/response frame pairs for renderer research. |

JavaScript baking and site templates
------------------------------------

Files in `config/sites` provide a small declarative replacer/template layer for
modern pages before they are converted to legacy OMS or native OM4. A template can click controls,
keep the useful page subtree, remove or unwrap elements, inject compatibility
CSS and run custom JavaScript. Page capture waits for both network and DOM
quiet, with a bounded 1.5–2.5 second default grace period so long polls do not
stall feature-phone requests. Optional `emojiAsImages` turns emoji glyphs into
small inline PNGs before the OMS renderer sees the page.

For one-off diagnostics, the same controls are available as request parameters:
`js=on`, `js_wait`, `js_idle`, `js_dom_idle`, `js_settle`, `js_timeout`,
`js_selector`, `js_script`, `js_final_script` and `js_emoji=1`. See
[`config/sites/README.md`](config/sites/README.md) and
[`config/sites/_example.json`](config/sites/_example.json) for the complete
per-host format.

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
- `origin/`, `presentation/`, `gateway/` – protocol-neutral pipeline contracts.
- `protocol/` – output encoders and full client-protocol adapter contracts.
- `transform/htmlmini/` – HTML/CSS transformer adapter.
- `oms/` – backwards-compatible Opera Mini facade and legacy renderer/codec code
  being migrated behind the new boundaries.
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
