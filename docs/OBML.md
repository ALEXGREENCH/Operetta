OBML (Opera Mini 2.06 Mod) — Stream Format (as implemented here)

Overview
- Transport header: 6 bytes
  - magic LE uint16: 0x3218
  - size BE uint32: total byte length including the 6‑byte header
- Body: DEFLATE-compressed payload of a V2 header + OBML tag stream
- Initial string: first field after the V2 header is a BE‑u16 length + bytes string with the canonical page URL (often prefixed with `1/`)

V2 Header Layout (oms/oms.go:313)
- Res1 [9]uint16: reserved
- TagCount uint16 (LE, byte‑swapped semantics) — count of payload tags (see Normalize)
- PartCurrent uint16 (LE, byte‑swapped) — current part (for pagination), 0x0100 for “1”
- PartCount uint16 (LE, byte‑swapped) — total parts, 0x0100 for “1”
- Res2 uint16: reserved
- StagCount uint16 (LE, byte‑swapped) — string count; conservative clients favor 0x0400
- Res3 uint16: reserved
- Res4 uint8: reserved
- Cachable uint16: 0xFFFF to indicate cacheable
- Res5 uint16: reserved

V2 Offsets (in deflated payload before compression)
- 0..17: Res1 (9 x u16, big-endian as viewed by client)
- 18..19: TagCount (as client reads it, big-endian; server stores little-endian byte-swapped)
- 20..21: PartCurrent (same byte-swap convention)
- 22..23: PartCount (same)
- 24..25: Res2
- 26..27: StagCount (same byte-swap convention)
- 28..29: Res3
- 30:     Res4 (1 byte)
- 31..32: Cachable
- 33..34: Res5
- 35..   : Initial page URL string (BE‑u16 + bytes), then tag stream

Strings
- Encoded as BE‑u16 length followed by raw bytes (UTF‑8 from server). See `AddString` in oms/oms.go:63.

Core Tags (byte values) and Payloads
- 'T' — Text node
  - Server encoding: 'T' + BE‑u16 length + bytes (UTF‑8)
  - Client may also accept a width/metrics prefix (observed in client), but server variant works with OM 2.06 Mod.
- 'B' — Line break (no payload)
- 'V' — Paragraph separator (no payload)
- '+' — Block separator/heading mark (no payload)
- 'L' — Link begin
  - Payload: BE‑u16 + URL string (typically absolute prefixed with `0/`)
  - Encloses link content; must be closed by 'E'
- 'E' — Link/end marker (no payload)
- 'R' — Horizontal rule
  - Payload: BE‑u16 encoded color (RGB565‑like per `calcColor`) (oms/oms.go:141)
- 'S' — Style
  - Payload: BE‑u32 bitmask (bold/italic/underline/center/right). See constants in oms/oms.go:23
- 'D' — Text color
  - Payload: BE‑u16 encoded color (same as 'R')
- 'I' — Inline image
  - Payload: BE‑u16 width, BE‑u16 height, BE‑u16 dataLen, BE‑u16 reserved(0), then `dataLen` bytes of encoded image (JPEG/PNG)
- 'J' — Image placeholder
  - Payload: BE‑u16 width, BE‑u16 height

Example: Minimal Page (hex dump)
- Logical content: Title "Hello", one paragraph with link to example.com
- OBML (deflated payload shown as first/last bytes via server’s dumpOMS):
  - V2: tag_count (swapped), part=1/1, stag=swapped
  - Tags: '+' 'T' 'B' 'L' 'T' 'E' 'Q'
- Binary layout example:
  - 'T' 00 05 48 65 6c 6c 6f             (Hello)
  - 'B'
  - 'L' 00 16 30 2f 68 74 74 70 3a 2f 2f 65 78 61 6d 70 6c 65 2e 63 6f 6d (0/http://example.com)
  - 'T' 00 04 4c 69 6e 6b                 (Link)
  - 'E'
  - 'Q'
  - V2 and transport header wrap this payload.

Forms (controls)
- 'h' — Form begin
  - Strings: action (or "1"), then method flag (server uses "1")
- 'x' — Text input
  - Payload: 1 byte config (0), then name (str), value (str)
- 'p' — Password input — name (str), value (str)
- 'i' — Hidden input — name (str), value (str)
- 'c' — Checkbox — name (str), value (str), 1 byte checked (0/1)
- 'r' — Radio — name (str), value (str), 1 byte checked (0/1)
- 'u' — Submit — name (str), value (str)
- 'b' — Button — name (str), value (str)
- 'e' — Reset — name (str), value (str)
- 's' — Select begin — name (str), 1 byte multiple (0/1), BE‑u16 optionCount
- 'o' — Option — value (str), label (str), 1 byte selected (0/1)
- 'l' — Select end

Other/Observed
- 'k' — Auth data (prefix/code), payload: 1 byte type (0=prefix,1=code) then string
- 'Y'/'y' — Font selection (client side); 'z' — x‑offset; seen in client renderer
- 'Q' — End of stream marker (server appends); clients expect it to be counted

Transport and Normalization (oms/oms.go:328, 369)
- Server finalizes as: append 'Q' → compute tag_count conservatively → write V2 (byte‑swapped fields) → deflate → 6‑byte header
- `NormalizeOMS` ensures conservative values: last tag 'Q', `tag_count = parsed+1`, `stag_count = 0x0400` (oms/oms.go:380)

Pagination (Parts)
- Server can split tag stream into parts by tag budget (default 1200), preserving the initial URL string per part.
- V2 fields PartCurrent/PartCount are set accordingly (oms/oms.go:352).
- Optional navigation links can be appended by server (prev/next) when HTTP server base is known (oms/oms.go:2289).

User-Agent and Site Profiles
- Per‑site configuration resides in `config/sites/<host>.json` (`main.go:516`, `main.go:538`).
- Schema: `{ "mode": "full|compact", "headers": { "Header-Name": "Value", ... } }`.
- Example templates provided for `google.com` and `google.ru` to force IE5 UA (see `config/sites/google.com.json`, `config/sites/google.ru.json`).

Notes
- Strings are produced as UTF‑8 on the server (legacy pages are transcoded from cp1251/KOI8‑R).
- Link URLs generally use the `0/` prefix for absolute targets, matching client expectations.
- Some server encodings are conservative variants of what the client supports; OM 2.06 Mod is tolerant.

Testing Tips
- Server endpoint `/validate?url=...` returns analysis including magic/size and a compact rendering.
- To tune pagination without code changes set `OMS_PAGINATE_TAGS` env or use `/fetch?pp=...&page=...`.
- Image cache size can be adjusted by `OMS_IMG_CACHE_MB` (default 200).
