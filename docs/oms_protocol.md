OMS Protocol Reference

Overview

- Purpose: compact binary format for pages rendered by legacy Opera Mini–like clients.

Framing

- Header OMS_HEADER_COMMON (6 bytes):
  - magic (2 bytes, little-endian): 0x3218
  - size (4 bytes, big-endian): total response length in bytes (6 + length of compressed body)
- Body: raw DEFLATE (no zlib header). First uncompressed bytes are OMS_HEADER_V2 (little‑endian), followed by tag stream. Payload ends with tag 'Q'.

Uncompressed header OMS_HEADER_V2 (little‑endian)

- res1[9] (u16×9): zeros
- tag_count (u16): byte-swapped (count low/high swapped)
- part_current (u16): 0x0100
- part_count (u16): 0x0100
- res2 (u16): 0
- Stag_count (u16): 0x0400
- res3 (u16): 0
- res4 (u8): 0
- cachable (u16): 0xFFFF
- res5 (u16): 0

Primitive encodings

- OMS_STRING: length (u16, big-endian) + bytes (UTF‑8; client reads via DataInputStream.readUTF; ASCII compatible)
- Color16: BGR565 (BE on the wire)
- Style32: 4 bytes big‑endian
- Bool: one byte 0/1

Tag Vocabulary (payload)

Text/Layout

- 'T': Text → OMS_STRING (skip empty; trim leading CR/LF)
- 'B': Line break
- 'V': Paragraph
- '+': Block separator
- 'D': Background color → u16 (BGR565, BE)
- 'S': Style → u32 (BE)
- 'R': Horizontal line → u16 (BGR565, BE)

Links

- 'L': Link start → OMS_STRING (URL), followed by 'T' (label), 'B', then 'E'
- 'E': Link end

Auth/Meta

- 'k': Auth data → type (u8: 0 prefix, 1 code) + OMS_STRING
- 'Q': End of page (must be present before compression ends)

Forms

- 'h': Form header → OMS_STRING action (or "1"), then OMS_STRING "1"
- 'x': Text input → cfg (u8, 0), OMS_STRING name, OMS_STRING value
- 'p': Password → OMS_STRING name, OMS_STRING value
- 'u': Submit → OMS_STRING name, OMS_STRING value
- 'i': Hidden → OMS_STRING name, OMS_STRING value
- 'b': Button → OMS_STRING name, OMS_STRING value
- 'e': Reset → OMS_STRING name, OMS_STRING value
- 'c': Checkbox → OMS_STRING name, OMS_STRING value, checked (u8)
- 'r': Radio → OMS_STRING name, OMS_STRING value, checked (u8)
- 's': Select begin → OMS_STRING name, multiple (u8), count (u16 BE)
- 'o': Option → OMS_STRING value, OMS_STRING label, selected (u8)
- 'l': Select end (used by some producers; readers may rely on count only)

Less common/unknown

- 'A' Anchor, 'C' SubmitFlag, 'F' rich field (id/width/data), 'Y'/'y' style reuse, 'z' indent,
  'M' alert, 'N' string, images/links 'I' 'J' 'K' 'X' 'Z' 'W' '^' '@' 'm' '\x08' '\x09'.
  These are not required for basic text/forms rendering and are currently not produced by the Go server.

Go Implementation (go/oms)

- Framing: magic/size/deflate as above; 'Q' appended before compression.
- V2 header fields and tag_count swapping match C.
- Tags implemented: T,B,V,+,D,S,R, L/E, k, h, x, p, u, i, b, e, c, r, s, o (without 'l' by default).
- HTML mapping: title/body/br/hr/p/a/img/form/textarea/input/select/option; display:none textarea ignored. <img> becomes text "[Img]".

Validation checklist

- Header: 0x18 0x32 then size == len(response)
- Inflate from offset 6; first 35 bytes map to V2 (LE)
- Payload ends with 'Q'
- Any 's' has exactly 'count' 'o' entries; optional trailing 'l'
- Checkbox/Radio contain name/value + 1 byte flag; text/password/submit/hidden/button/reset carry two strings

Version and Compatibility

- Protocol Family: This implementation follows the “OM 2.xx”/OMS v2 family used by early Opera Mini J2ME clients (e.g., Opera Mini 2.06). The C headers (`oms.h`) explicitly distinguish OM 2.xx versus OM 3.xx for some tags (e.g., color/style payloads). It predates the newer OBML v6+ formats often described in modern community docs.
- Header: Uses an OMS v2 header (35 bytes, little‑endian) placed at the start of the uncompressed stream. The 6‑byte COMMON header precedes the compressed body. This differs from some OBML v6 write‑ups that assume a different framing/versioning.
- Strings/Encoding: Strings are length‑prefixed (u16 big‑endian) and bytes are treated as UTF‑8 for ASCII; the legacy client reads via `DataInputStream.readUTF`.
- Colors/Style: Colors are 16‑bit BGR565 (BE on the wire) for OM 2.xx; the 32‑bit style field is written big‑endian. OM 3.xx alternatives (24/32‑bit color) are not produced.
- Images/Media: Image tags `I/J/K/X/Z/W/^/@/m/\x08/\x09` are not emitted; `<img>` is rendered as text `[Img]`, matching the legacy C xml walker.
- Select/Option: We emit `s` + `o…o` and an explicit `l` (select end) for conservative compatibility with older parsers.
- Tag Count semantics: The original C code writes `v2.tag_count` as a byte‑swapped internal counter (which includes the trailing `'Q'`). Some early clients appear to use `tag_count` to size arrays and then still write a sentinel at index `tag_count`, which can crash if it exactly matches the number of real tags. Our implementation computes `tag_count` from the payload and may bump it by +1 for compatibility with Opera Mini 2.x. The payload still ends with `'Q'` inside the compressed body. If you target other clients, you can adjust this behavior.

Proxy Handshake (Opera Mini 2.x)

- Some OM2 clients POST to the root path (`/`) with `Content-Type: application/xml` but a body that is a null‑separated list of `key=value` pairs (not actual XML). Typical keys:
  - `u`: target path, e.g. `/obml/0/http://example.com/` or `/obml/https://example.com/`
  - `k`: accept mime (e.g., `image/jpeg`), `o`: width, `q`: locale, `v`: client version, `i`: user‑agent, `A/B/C/D/E`: platform strings, `d/g/b/y/t/w/e`: device and misc flags
- Server handling:
  - Parse the body by splitting on `\x00`; extract `u`.
  - Normalize `u` to a real URL by removing `/obml[/<ver>]/` and prepending `http://` if no scheme is present.
  - Download the page and respond with an OMS payload (`application/octet-stream`) with explicit `Content-Length` and `Connection: close`.
  - Do not use chunked transfer; some MIDP stacks fail on it.

Transport Requirements (from legacy J2ME sources)

- HTTP over TCP (no UDP). Clients use `javax.microedition.io.HttpConnection` and sometimes raw sockets via `Connector.open("socket://…")` for proxy hops.
- HTTP/1.1 semantics; requests often set:
  - `User-Agent: Opera/8.01 (J2ME/MIDP; Opera Mini/2.x …)`
  - `Accept: */*`
  - `Connection: close` (and sometimes `Proxy-Connection: close` when going via gateway)
  - `Host: <authority>` and occasionally `X-Online-Host: <authority>` (proxy tunneling header)
  - Optional `Range`/`Accept-Ranges` for file downloads (not required for OMS pages)
  - `Content-Type: application/xml` only for proxy POST flows (not relevant to OMS fetch)
- Server expectations for OMS downloads:
  - Respond with HTTP/1.1 200 OK
  - Include `Content-Type: application/octet-stream`
  - Include explicit `Content-Length` (clients may fail on chunked transfer)
  - Prefer `Connection: close` to avoid keep‑alive issues on MIDP stacks
  - Use ports allowed by the environment; some builds restrict 80/443/8080 to trusted MIDlets. If untrusted, use alternate ports or sign the app.


