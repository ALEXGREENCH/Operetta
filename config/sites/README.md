# Per-site legacy rewrites

Operetta reads optional JSON files named after the target host from this
directory. For `m.example.com`, it tries `m.example.com.json`, then
`example.com.json`, then `com.json`. Set `OMS_SITES_DIR` to use another
directory.

Files are re-read when their size or modification time changes, so templates
can be tuned while the server is running. Removing a file also takes effect on
the next request.

The `rewrite` block is the declarative successor to Opera Mini's per-site
replacer templates. The same acquisition stage feeds legacy OMS and native
OM4. It runs in Chromium after the page is loaded and before the bounded settle
period:

- `clickSelectors` clicks matching controls, useful for consent or “load more”;
- `mainSelector` keeps one main subtree and discards the rest of the body;
- `removeSelectors` removes matched elements;
- `unwrapSelectors` removes wrappers while preserving their children;
- `css` injects a final style override before the DOM snapshot.

Declarative cleanup runs twice: once before settling (including
`clickSelectors`) and once immediately before capture. The final pass does not
repeat clicks, but it removes late-injected matches, unwraps late wrappers and
refreshes the compatibility CSS without adding duplicate `<style>` elements.

The `bake` block controls JavaScript execution and settling. Defaults are a
minimum 1500 ms grace period, 350 ms of network quiet, 350 ms of DOM quiet and
a maximum of 2500 ms. Event streams and media downloads do not keep the page
open indefinitely. `scripts` is an escape hatch for sites that need custom DOM
logic before settling; `finalScripts` runs synchronous last-mile cleanup after
settling. `emojiAsImages` converts rendered emoji into tiny inline PNG images so
clients without suitable fonts can display them.

See `_example.json` for all supported fields. Copy it to `<host>.json` and
remove options that are not needed. The `_example.json` filename cannot match
a real web host and is not loaded automatically.
