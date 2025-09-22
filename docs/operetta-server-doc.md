# Operetta Server Documentation

## Overview
Operetta is a Go-based reimplementation of the Opera Mini 1.x–3.x gateway, tuned for the 2.06 modded client. It accepts legacy Opera Mini (OM) POST handshakes, fetches upstream HTML, and produces OMS/OBML v2 byte streams that the client can render. This guide explains the server architecture, the OBML encoding it emits, and the specifics of the Opera Mini ↔ Operetta protocol.

## Repository Layout
- `README.md` — Short project summary and feature list.
- `PROTOCOL.txt` — Annotated capture of a legacy Opera Mini POST handshake and the binary OMS reply.
- `main.go` — HTTP entrypoint that registers handlers (`/`, `/fetch`, `/validate`, `/ping`), parses Opera Mini payloads, coordinates caching/pagination, and invokes the `oms` renderer.
- `url.go` — Helpers for URL normalization, bookmark fallbacks, cache keys, and query composition.
- `oms/oms.go` — Core rendering engine: HTML fetch/decode, CSS heuristics, DOM walker, OBML tag emitters, pagination helpers, image pipeline, and OMS finalisation.
- `config/sites/` — Host-specific overrides (`mode`, `headers`) loaded per target; override the directory via `OMS_SITES_DIR`.
- `docs/` — Background notes (`OBML.md`, `oms_protocol.md`) plus this guide.
- `dist/`, `build.ps1`, `build.sh`, `Makefile` — Build artifacts and helper scripts.

## Runtime Architecture
1. `main()` reads the `-addr` flag or `PORT`, configures logging, constructs the HTTP mux via `newServer()`, and starts `net/http.Server`.
2. The mux routes Opera Mini POST requests to `rootHandler`, manual fetches to `fetchHandler`, diagnostics to `validateHandler`, and health checks to `pingHandler`.
3. `rootHandler` parses the null-separated key/value payload (`parseNullKV`), normalises the requested URL (`normalizeObmlURL`), prepares `oms.RenderOptions` from client hints (`k`, `d`, `j`, auth tokens), and calls `loadWithSiteConfig`.
4. `loadWithSiteConfig` merges per-site overrides, selects between `oms.LoadPageWithHeadersAndOptions` and `oms.LoadCompactPageWithHeaders`, and hands control to the renderer which produces an `*oms.Page`.
5. The handler finalises response headers (`Content-Type: application/octet-stream`, explicit `Content-Length`, `Connection: close`), logs abbreviated OMS diagnostics via `dumpOMS`, writes cookies from `page.SetCookies`, and streams the packed OMS binary back to the client.

## Opera Mini Handshake
Opera Mini 2.x sends `Content-Type: application/xml`, but the body is a null-delimited list of `key=value` pairs. Operetta reads them directly, applies device hints, and echoes Opera’s authentication tokens in the response.

```text
POST / HTTP/1.1
Host: 192.168.0.3:8008
Content-Type: application/xml
...
k=image/jpeg\x00
o=280\x00
u=/obml/http://operamini.com/\x00
q=ru\x00
v=Opera Mini/2.0.4509/hifi/woodland/ru\x00
i=Opera/8.01 (J2ME/MIDP; Opera Mini/2.0.4509/1630; ru; U; ssr)\x00
...
j=opf=1&q=Yukaba&btnG=Search+in+Google
```

| Key | Meaning |
| --- | --- |
| `k` | Preferred image MIME type (`image/jpeg`, etc.). |
| `o` | Gateway/version discriminator (OM 2.x uses 280; OM 3.x uses 285). |
| `u` | Requested resource path (usually `/obml/<scheme>/<URL>`); Operetta normalises it to an absolute URL. |
| `q` | UI locale code used for language-specific tweaks. |
| `v` | Opera Mini client version string reported by the handset. |
| `i` | Desktop-equivalent user agent string for compatibility heuristics. |
| `s` | Legacy session slot indicator (normally `-1`). |
| `n` | Request counter / page sequence marker. |
| `A` | CLDC profile level (for example `CLDC-1.1`). |
| `B` | MIDP profile level (`MIDP-2.0`). |
| `C` | Device identifier (model/firmware string). |
| `D` | Device UI language code. |
| `E` | Preferred character encoding (for example `ISO-8859-1`). |
| `d` | Capability block (`w` width px, `h` height px, `c` colours, `m` heap KB, `i` images on/off, `q` image quality, `f/j/l` extra flags). |
| `c` | Authentication code (hash) used by Opera to validate responses. |
| `h` | Authentication prefix paired with `c`. |
| `f` | Referrer URL from the client. |
| `g` | Gateway feature flag (`1` enables full proxy flow). |
| `b` | Client modification tag (for example `mod2.06`). |
| `y` | Secondary language code (content preference). |
| `t` | Phone-number auto-detection toggle (`0` disables linking). |
| `w` | Multipart indicator `partCurrent;isLast` for paginated pages. |
| `e` | Compression hint: `def` (deflate) or `none`. |
| `j` | URL-encoded form payload appended on submission. |

**Response.** Operetta replies with `HTTP/1.1 200 OK`, sets `Content-Type: application/octet-stream`, always provides `Content-Length`, and closes the connection to satisfy MIDP client expectations. The body is the packed OMS binary described below.

## HTTP Endpoints
- `POST /` — Primary Opera Mini ingress: handles the handshake, internal `server:` pages, local bookmark fallbacks, and OBML generation.
- `GET /fetch` — Diagnostic/manual entry point that mirrors proxy behaviour for a given URL; accepts `url`, `action`, `get`, `ua`, `lang`, `img`, `hq`, `mime`, `maxkb`, `pp`, and `page` parameters.
- `GET /validate` — Fetches the target twice (full and compact), normalises both, and returns JSON with `analyzeOMS` metrics.
- `GET /ping` — Lightweight liveness probe that returns `pong`.

## Rendering Pipeline
- **Fetch & request shaping.** `LoadPageWithHeadersAndOptions` / `LoadCompactPageWithHeaders` build the origin request, apply per-site header overrides, forward cookies and referer, switch to POST when `RenderOptions.FormBody` is present, and force gzip-only `Accept-Encoding` to avoid Brotli.
- **Charset handling.** `decodeLegacyToUTF8` inspects `Content-Type` and `<meta charset>` hints, converting Windows-1251 and KOI8-R bodies to UTF-8 before parsing.
- **Stylesheet assembly.** `buildStylesheet` collects inline `<style>` blocks and up to three linked stylesheets, normalises simple CSS properties, and feeds them to `computeStyleFor` for decisions such as `display:none` and colours.
- **DOM traversal.** The recursive `walkRich` walker skips hidden nodes, recognises structure (`p`, headings, lists, `hr`/`br`), emits OBML tags, and ensures headings become bold separators via `AddPlus` and style flags.
- **Text & styles.** Text nodes become `T` tags with UTF-8 payload; `walkState` tracks style bits (`styleBoldBit`, `styleItalicBit`, `styleUnderBit`, `styleCenterBit`, `styleRightBit`) and emits `S` tags when the active style changes.
- **Forms & controls.** `<form>` (`h`), `<input>` (`x`, `p`, `i`, `u`, `b`, `e`, `c`, `r`), and `<select>` (`s`, `o`, optional `l`) are rendered, mirroring Opera’s expectations and echoing submitted payload via `RenderOptions.FormBody`.
- **Images.** `fetchAndEncodeImage` obeys `RenderOptions.ImagesOn`, uses in-memory and optional disk LRU caches (`OMS_IMG_CACHE_DIR`, `OMS_IMG_CACHE_MB`), converts to JPEG/PNG as requested, rescales with `golang.org/x/image/draw`, and emits `I` tags; oversized or disabled images fall back to `J` placeholders.
- **Pagination & navigation.** `RenderOptions.MaxTagsPerPage` (defaulted by `OMS_PAGINATE_TAGS`) splits payloads via `splitByTags`; navigation fragments are appended when `RenderOptions.ServerBase` is known. Packed snapshots land in `Page.CachePacked` for reuse by `SelectOMSPartFromPacked`.
- **Finalisation & normalisation.** `Page.finalize()` appends the terminal `Q`, computes conservative tag/string counts (tunable via `OMS_TAGCOUNT_MODE` / `OMS_TAGCOUNT_DELTA`), writes the V2 header, deflates the payload, and prefixes the transport header. `NormalizeOMS` / `NormalizeOMSWithStag` repack responses to stabilise counts (e.g., force `stag_count = 0x0400`).
- **Auth echo & cookies.** The renderer mirrors `AuthCode` / `AuthPrefix` into `k` tags, records origin `Set-Cookie` values, and exposes them through `page.SetCookies` so the HTTP layer forwards them to the client.

## OBML / OMS Format Details
- **Transport header.** Each response begins with 6 bytes: little-endian magic `0x3218` plus a big-endian 32-bit length covering header and compressed body.
- **V2 header fields.** The deflated stream starts with a 35-byte V2 header containing byte-swapped `TagCount`, `PartCurrent`, `PartCount`, `StagCount`, and `Cachable=0xFFFF`. Operetta mirrors the legacy C implementation by swapping bytes (`swap16`) and counting the trailing `Q`.
- **Strings & encoding.** Strings are big-endian length-prefixed UTF-8 blobs; the first string after the header is the canonical page URL (for example `1/http://...`).
- **Colours & styles.** Colours use 16-bit BGR565 (`calcColor`), while styles use 32-bit masks stored big-endian. `AddBgcolor`, `AddTextcolor`, and `AddStyle` emit `R`, `D`, and `S` tags.
- **Compatibility.** Operetta targets OMS/OBML v2 as used by Opera Mini 2.x. Later OBML variants (v12–v16) with chunked sections, ARGB colours, or relative coordinates are not emitted.

| Tag | Payload | Meaning |
| --- | --- | --- |
| `+` | none | Block separator used for headings/sections. |
| `B` | none | Line break (`<br>`). |
| `V` | none | Paragraph separator (`<p>`). |
| `T` | length + UTF-8 bytes | Text node content. |
| `L` | length + URL string | Link start; closed by `E`. |
| `E` | none | Link end marker. |
| `R` | 2-byte colour | Horizontal rule / background colour segment. |
| `S` | 4-byte style mask | Style change (bold, italic, underline, align). |
| `D` | 2-byte colour | Text colour change. |
| `I` | width, height, dataLen, reserved, data | Inline image payload (JPEG/PNG). |
| `J` | width, height | Image placeholder when data is omitted. |
| `k` | type byte + string | Authentication data (`type=0` prefix, `1` code). |
| `h` | two strings | Form header (action, method marker). |
| `x` | cfg byte + two strings | Text input (name, value). |
| `p` | two strings | Password input. |
| `i` | two strings | Hidden input. |
| `u` | two strings | Submit button. |
| `b` | two strings | Generic button. |
| `e` | two strings | Reset button. |
| `c` | two strings + flag | Checkbox (name, value, checked). |
| `r` | two strings + flag | Radio button. |
| `s` | string + flags | Select start (name, multiple flag, option count). |
| `o` | two strings + flag | Option entry (value, label, selected). |
| `l` | none | Select end (emitted for compatibility). |
| `Q` | none | End-of-stream marker. |

## Caching, Pagination, and Auth Echo
- **Page cache.** `pageCache` (`sync.Map`) stores packed OMS responses keyed by URL plus rendering preferences (`cacheKey`), allowing `/fetch` to serve later pages via `cacheSelect` without refetching the origin.
- **SelectOMSPartFromPacked.** Inflates a cached response, splits it by tag budget, and returns the requested slice while updating part counters; errors fall back to the original payload.
- **Cookie propagation.** Rendered pages append upstream `Set-Cookie` headers to `page.SetCookies`; handlers forward them so Opera Mini persists origin cookies.
- **Auth tokens.** `RenderOptions.AuthCode` and `AuthPrefix` are echoed via `k` tags so the client accepts the stream.

## Configuration and Environment

| Variable | Description |
| --- | --- |
| `PORT` | Overrides the listen port (otherwise the `-addr` flag, default `:8080`). |
| `OMS_BOOKMARKS_MODE` | Controls `/obml/` bookmark fallback: `remote/pass` proxies opera-mini.ru; anything else serves the local list. |
| `OMS_BOOKMARKS` | Comma-separated `name|url` pairs for the local bookmark page. |
| `OMS_SITES_DIR` | Custom directory with per-host JSON configs. |
| `OMS_IMG_CACHE_DIR` | Path for on-disk image cache. |
| `OMS_IMG_CACHE_MB` | Memory/disk cache budget in megabytes (default 200). |
| `OMS_IMG_DEBUG` | When `1`, logs image download/conversion failures. |
| `OMS_TAGCOUNT_MODE` | Tag-count strategy (`exact`, `exclude_q`, `plus1`, `plus2`). |
| `OMS_TAGCOUNT_DELTA` | Numeric delta added to the computed tag count. |
| `OMS_PAGINATE_TAGS` | Default maximum tags per OMS part when pagination is enabled. |

`/fetch` also honours `img`, `hq`, `mime`, `maxkb`, `pp`, `page`, `ua`, and `lang`, which map directly onto `RenderOptions`. Per-site JSON files accept `{"mode":"full|compact","headers":{...}}`.

## Debugging and Tooling
- **Log dumps.** `dumpOMS` prints the OMS magic, size, and head/tail bytes for every response, aiding inspection.
- **Validator.** `/validate?url=...` renders full and compact variants, runs `analyzeOMS`, and reports tag counts, string counts, and pagination data in JSON.
- **Index helper.** The `GET /` HTML form (`indexHTML`) lets you test the server manually without Opera Mini.
- **Image tracing.** Set `OMS_IMG_DEBUG=1` to log cache hits/misses and conversion issues while fetching images.

## Compatibility Notes and Limitations
- **CSS scope.** Only a conservative subset of CSS is honoured (display, colour, background, simple inline styles); complex layouts, floats, and media queries are ignored.
- **Forms.** GET submissions are fully supported; POST bodies are proxied when `RenderOptions.FormBody` is provided, but multipart uploads and file inputs are not implemented.
- **Images.** Large images may be downgraded to placeholders based on `MaxInlineKB`; formats beyond JPEG/PNG (for example animated GIF or unsupported WebP) are stripped.
- **OBML coverage.** Tags beyond the OM 2.x baseline (multimedia tags, advanced font controls) are not emitted; clients needing OBML v6+ features require separate adaptation.
- **Transport.** Responses are always unchunked HTTP/1.1 with `Connection: close`; HTTPS support relies on external termination (reverse proxy or stunnel).

## Further Reading
- **`docs/OBML.md`** — Deep dive into tag layout, pagination, and transport header nuances used by Operetta.
- **`docs/oms_protocol.md`** — Legacy C/Java protocol reference that informed the Go port.
- **Community OBML spec** — [grawity/obml-parser – obml-format.md](https://github.com/grawity/obml-parser/blob/master/obml-format.md) documents later OBML versions for comparison.
