# SPIDER.md — building the site generator

The project name is **Anneal**.

Take a handful of seed URLs, crawl a live site, and write every response to disk in the
layout from `PLAN.md`, ready for nginx.

## 1. Split the problem in three

Do not build one tool. Build the smallest possible piece, and let mature tools do the
rest.

| stage | who does it | why |
|---|---|---|
| **crawl** — discover URLs, fetch, capture bytes | **an existing crawler** | solved problem: politeness, redirects, cookies, retries, robots, JS |
| **materialise** — URL + Content-Type → file path | **ours**, in Go | no existing tool writes the `PLAN.md` layout; see §3 |
| **verify** — re-serve and compare | **ours**, in Go | the only thing that proves the build is correct |

The seam between stage 1 and 2 is a **WARC file**.

End-to-end order:

1. **Seed** — collect URLs from the home page, `sitemap.xml`, `robots.txt`, and recent
   nginx access logs (§10). Normalise and dedupe them first.
2. **Crawl** — producer of your choice, output one or more WARCs (§4, §5). Keep the WARC.
3. **Materialise** — `spider materialise` → the tree, plus `extra.map`,
   `redirects.conf`, `manifest.json`, `report.md` (§6). Fix and re-run until it exits 0.
4. **Wire** — deploy the fragments into the `PLAN.md` §2 config (`extra.map` is
   `include`d at `http` level; the chain probes `$resolved` first).
5. **Verify** — re-serve the tree and compare against the live site (§9). This is the
   gate, not a formality.
6. **Deploy** — only after verify is clean, and only the output root.

## 2. Why a WARC is the right interface

A WARC stores, per URL: the exact request URL, status, **all response headers including
`Content-Type`**, the raw body bytes, and a payload digest. That is precisely the input
the naming rule needs — and nothing else.

- No crawler-specific output format. `wget`, `browsertrix-crawler`, `wabac`, and
  ArchiveBox all emit WARC, so swapping crawlers cannot affect the materialiser.
- Bodies are byte-exact. No re-encoding, no whitespace normalisation, no charset games.
- `Content-Type` comes from the server, which is the only trustworthy source for the
  extension we append.
- The crawl is reproducible and auditable: keep the WARC, re-materialise later without
  touching the live site again.

## 3. Why you cannot just use the crawler's own output

`wget --mirror` writes a plausible-looking tree that is wrong in three ways that
silently lose data. Measured against an 8-URL test site in the `PLAN.md` layout:

```
wget mirror                      ours
127.0.0.1:8300/index.html        _index.html
127.0.0.1:8300/x.html            x.html            <- /x
127.0.0.1:8300/x/index.html      x.html.html       <- /x.html
127.0.0.1:8300/theme.php?v=3     x/_index.html
127.0.0.1:8300/thumb.php?id=5    theme.php?v=3.css
127.0.0.1:8300/img.png           thumb.php?id=5.png
127.0.0.1:8300/s.css             img.png.png
                                 s.css.css
```

1. **It collides and destroys a page.** `wget` appends `.html` to `/x` because the
   response is HTML, so `/x` and `/x.html` both target `x.html`. Result: **8 URLs
   went in, 7 files came out**, and `/x.html` overwrote `/x`. No warning.
2. **It writes no extension when the URL has none** — `theme.php?v=3` has no `.css`, so
   nginx types it `application/octet-stream` and the browser downloads it.
3. **It does not double extensions**, which is exactly what keeps `/x` and `/x.html`
   distinct in this design.

`httrack` has its own layout, same class of problem. So: use the crawler for the
**WARC**, and do the naming yourself.

## 4. Recommended crawlers

| crawler | use it for | notes |
|---|---|---|
| **`wget --warc-file`** | first choice: classic PHP/static sites | installed everywhere, no runtime, complete for HTML-referenced resources. Verified working (§5) |
| **`browsertrix-crawler`** | JS-heavy sites: SPA, lazy-load, XHR-rendered | headless Chromium + WARC output; Docker image. Solves the rendering problem *and* the WARC problem |
| **`katana`** | URL discovery only | parses JS to find endpoints `wget` never sees; feed its URL list as seeds |
| **`wget2` / `httrack`** | alternatives to wget | same WARC-or-not tradeoff |
| **`sitemap.xml` + `robots.txt`** | seed list | free, and the best way to find unlinked pages |
| **nginx access logs** | seed list | the ground truth for "what URLs actually get requested" |

```
wget --recursive --level=inf --page-requisites --no-parent \
     --delete-after --no-warc-keep-log \
     --warc-file=site --warc-cdx \
     -P /tmp/scratch \
     https://example.com/
```

`--delete-after` keeps nothing on disk (the WARC is the output); `--warc-cdx` also
writes a CDX index — a fast, greppable `url / content-type / status / digest` list that
is worth reading before you materialise anything.

**`wget` limits to know:** it fetches what the HTML references (img, script, css,
frames) and, under `--page-requisites`, resources referenced from CSS. It does **not**
run JavaScript, so XHR/`fetch` JSON, lazy-loaded images and client-side routes are
invisible. It also respects `robots.txt` — use `-e robots=off` only on your own server.

If the site needs JS, use `browsertrix-crawler` (still WARC) and skip to §6.

## 5. When you do need a real browser

Two cases, both narrow:

- **The site renders with JS.** Then the crawler must be browser-based; use
  `browsertrix-crawler` and keep the WARC seam. Only hand-roll Playwright if you cannot
  run its container, and then emit WARC yourself (`warcio.WARCWriter`) so the
  materialiser is unchanged.
- **Discovery only.** `page.on('response')` is the only way to enumerate XHR endpoints.
  Scrape *those URLs* with Playwright, then feed them to `wget` as seeds so the byte
  capture stays in one place.

Playwright traps, if you hand-roll: await `response.body()` inside the handler (bodies
are released on navigation), dedupe on the response URL not the requested URL, treat
`networkidle` as a heuristic, block off-origin requests, and disable service workers.

## 6. The materialiser — Go

Written in **Go**, as a single static binary. Rationale:

- **No runtime to install.** `CGO_ENABLED=0 go build` produces one file you can scp to
  a build server or CI runner. Python needed an interpreter plus `warcio`; Node needs a
  runtime plus `node_modules`. This tool needs neither.
- **Zero third-party packages.** Everything below is standard library. A WARC reader is
  ~40 lines (§6.1); the format is a header block, a `Content-Length`, a payload, and
  four trailing bytes, and Go's `compress/gzip` already concatenates multi-member
  gzipped WARCs.
- **Go strings hold arbitrary bytes**, so binary bodies cannot be corrupted by a
  string round-trip the way they can in JS/TS.

Layout:

```
spider/
  main.go         CLI: crawl | materialise | verify
  warc.go         WARC reader (stdlib only)
  mime.go         mime.types -> extension, canonical extension per type
  naming.go       URL -> file path (string operations only, see §6.2)
  materialise.go  tree + extra.map + redirects.conf + manifest.json + report.md
  verify.go       served tree vs manifest
  crawl.go        thin wget wrapper
```

CLI:

```
spider crawl       --origin https://example.com --warc site [--seed /sitemap.xml …]
spider materialise --warc site.warc.gz --origin https://example.com \
                   --root ./build/example.com --out ./build/example.com/nginx \
                   --mime ./nginx/mime.types
spider verify      --base http://127.0.0.1:8080 \
                   --manifest ./build/example.com/nginx/manifest.json
```

`materialise` emits into `--out`:

| file | purpose |
|---|---|
| `extra.map` | `map $request_uri $resolved` for URLs the nginx chain cannot resolve |
| `redirects.conf` | `301`s recorded in the crawl |
| `manifest.json` | `url → file, ct, ext, status, bytes, sha256` |
| `report.md` | stored / skipped / map entries / hashed, and **unmapped Content-Types** |

Exit codes: `0` clean, `1` verify failures, `2` a `Content-Type` had no extension. The
`2` is deliberate — an unmapped type becomes `application/octet-stream` in production;
fix it in `nginx/mime.types` (`PLAN.md` §2.1) and re-run.

### 6.1 WARC reading

Only a **reader** is needed, because the crawl is done by `wget` or `browsertrix`, which
write the WARC. Read the gzip stream, then loop: `WARC/1.0` line, headers until a blank
line, `Content-Length` bytes of payload, four trailing bytes. A response record's
payload is an HTTP header block, a blank line, then the body.

Two details worth getting right: parse the status by finding the 3-digit token in the
status line (not by field index — `HTTP/1.1 404 Not Found` has a different shape than
`200 OK`), and take the body as everything after the first `\r\n\r\n`.

### 6.2 Go-specific rules (verified traps)

**Never rebuild the URI with `net/url`.** The instinctive move for "append the
extension" is `u.Path += ".html"`. It changes the bytes:

```
/a%7Eb   -> /a~b.html        different filename
/a%2fb   -> /a/b.html        %2f became a DIRECTORY SEPARATOR
/p?q=%2B -> /p.html?q=%2B    extension in the wrong place entirely
```

Parse-and-rebuild re-encodes; the filename must equal the raw request URI. Strip the
origin by **string slicing** and concatenate the extension:

```
/a%7Eb   -> /a%7Eb.html      correct
/p?q=%2B -> /p?q=%2B.html    correct
```

**Never use `filepath.Join` for the destination.** It cleans the path:

```
filepath.Join("/srv/build", "/x/")  = "/srv/build/x"    <- trailing slash stripped
filepath.Join("/srv/build", "/a//b") = "/srv/build/a/b"
```

The trailing slash is what selects the `_index.html` case, so cleaning silently writes
a file named `x` instead of `x/_index.html`. Same for `path.Clean`. Build by
concatenation, and use a hand-written `dirOf` rather than `filepath.Dir` (which also
cleans) when creating parent directories.

**Refuse paths containing `..`.** A URL segment `..` would let a crawl write outside the
output root. Skip, and list it in `report.md`.

**Write `[]byte`, never a string.** In Go this is safe by construction — the note exists
because it is the single most destructive mistake in the JS/TS equivalent, where a
string round-trip turns `0x89` into `U+FFFD` and silently corrupts every image.

**Key `extra.map` on `$request_uri`, never `$uri`.** `$uri` has the query string
stripped, and every URL needing a map entry has a query string. This is a silent
failure: the entry simply never matches and the URL 404s.


## 7. Naming rule (must be exact)

```
path = root + <pathname> + <search> + "." + <ext>      # ordinary URL
path = root + <pathname> + "_index." + <ext>           # URL ends in "/"
```

| URL | file |
|---|---|
| `/x` | `x.html` |
| `/x.html` | `x.html.html` |
| `/x/` | `x/_index.html` |
| `/theme.php?v=3` (CSS) | `theme.php?v=3.css` |
| `/img.png` | `img.png.png` |
| `/search?q=a&page=2` | `search?q=a&page=2.html` |

- **Always append the extension**, even when the URL already has one. This is what keeps
  `/x` and `/x.html` in separate files. It is the whole point; do not optimise it away.
- **`<search>` keeps the leading `?`**, stored **raw/undecoded**, because nginx's
  `$request_uri` is the undecoded request line. Store the bytes the client sent.
- **`<ext>` comes from `Content-Type`**, never from the URL. Invert
  `nginx/mime.types` once; keep a canonical extension per type
  (`text/html → html`, `image/jpeg → jpg`).
- **Path ≥ 250 bytes** → `sha1(url)[:16] + ".ext"` under `_hash/`, plus a `map` entry.
- **Residue** — the URL's apparent extension differs from the true one *and* the true
  extension is outside the probed set: emit a `map` entry. `/theme.php?v=3` → `css` is
  probed, fine. `/thumb.php?id=5` → `png` is not, so it needs a map entry.

## 8. nginx wiring

The generated fragments plug into the `PLAN.md` §2 config:

```nginx
include /etc/nginx/extra.map;              # generated
location / {
    try_files $resolved $request_uri.$ext $request_uri.html $request_uri.css
              $request_uri.js $request_uri.json $uri $uri/ =404;
}
```

The `map` is keyed on **`$request_uri`**, not `$uri`: `$uri` has the query string
stripped, and every residue case has a query string. This is a silent failure if you get
it wrong — the entry never matches and the URL 404s.

## 9. Verify pass — not optional

Never ship on "the crawl finished". Re-serve the generated tree with the real nginx
config, then let `spider verify` compare every URL in the manifest against what the crawl
captured:

```
for each URL in manifest.json:
    GET <base><path> from the generated tree
    assert status       == manifest.status
    assert sha256(body) == manifest.sha256
    assert Content-Type == manifest.ct
```

`--base` points at the running nginx. Fetching the served tree (not the files) is the
point: it exercises the whole `try_files` chain, the `map`s, and the MIME table. Exit
code `1` on any failure.

When building the request, keep the raw path — in Go, set `req.URL.Opaque` (or build the
URL string directly) so the client does not re-encode `%2F` and friends; a re-encoded
query reaches a different file and reports a false failure.

Also assert no directory-style URL returns `403` (a section with no `_index.html` is a
403, not a listing).

Then act on mismatches:

| mismatch | action |
|---|---|
| same body at `/x` and `/x/` | `301` to the canonical form (`PLAN.md` §3) |
| resolution went through nothing | add to `extra.map` |
| resolved but wrong type | check the probe order (`PLAN.md` §2.2) |
| served as `octet-stream` | `Content-Type` missing from `nginx/mime.types` |
| anything else | **build failure** |

Both real defects found while building this were caught here, not by the crawler: the
missing `$request_uri.$ext` probe (every image, font and video 404'd) and the `$uri`
vs `$request_uri` map key (the residue entry silently never matched).

## 10. Seeds

Pick seeds deliberately; the crawler only finds what is reachable:

- the home page
- `sitemap.xml` URLs (parse it — often the only list of unlinked pages)
- every URL in the last 30 days of nginx access logs
- known query-string entry points (`/index.php?page=`, `/search?q=`)

Normalise before crawling: drop `#fragment`, drop `?utm_*`/`fbclid`/`gclid` variants,
strip session ids (`PHPSESSID=`) — they generate infinite distinct URLs. Log what you
dropped.

**Decide the fate of extra params up front.** Filenames match the request URI exactly, so
`?bla=whatever&utm_source=x` is a different file from `?bla=whatever`, and will `404`
unless a file was generated for it. Three options: crawl those variants too (safest),
add a `301` to the canonical URL, or accept the 404. Do **not** try to fix this by
truncating the query string — that silently serves the wrong page for any URL with two
meaningful params.

## 11. What cannot be static

Detect and report; do not pretend:

| thing | handling |
|---|---|
| `POST` forms | not servable statically — list in `report.md` |
| server-side search, filters, pagination | needs JS search (Pagefind) or a function |
| login / personalised pages | crawl anonymously; report protected areas |
| `Set-Cookie` on a page response | content may differ per visitor — flag it |
| infinite scroll / "load more" | needs interaction scripting, or it silently does not exist |
| pages with nonces/timestamps | churn the manifest every crawl — normalise or accept |

## 12. Manifest and rebuilds

`manifest.json` drives incremental work. On re-crawl diff by URL: new → write, same
`sha256` → skip, changed → overwrite and log, gone → delete **inside the output root
only**. Keep the previous manifest to report what changed between builds.

## 13. Building and deploying

```
CGO_ENABLED=0 go build -trimpath -o spider ./spider
```

Static binary, no runtime, no `node_modules`, no venv — copy it to the build host or CI
runner and run it. `GOOS=linux GOARCH=amd64` for a target that differs from the build
machine. Cross-compile to anything Go supports; nothing in this tool is platform
specific except the `?`-in-filename assumption, which is Linux/macOS only.

## 14. Operating notes

- **Crawl staging, not production**, if the site has side effects (view counters, carts,
  rate limits).
- **Keep the WARC.** It is the audit trail and it lets you re-materialise with a fixed
  naming rule without re-crawling.
- **Respect `robots.txt` and rate limits** on anything you do not own.
- **Quote every path in build scripts.** Filenames contain `?`, `&`, `=`, `%`, and
  spaces: use `find -print0`, `xargs -0`, `tar --null`, `"$file"` everywhere. A bare
  `for f in *.css` mangles them.
- **Never `rm -rf` a directory you did not create** — wipe only the configured output
  root, checked against an absolute path.

## 15. Limits

- Third-party assets (CDN fonts, analytics, embeds) stay remote, since nothing is
  rewritten. Mirroring them means a rewrite pass — a separate decision.
- Client-side-only routes are invisible unless seeded directly.
- Page content that varies per request (A/B tests, personalisation) freezes to whichever
  variant the crawl saw.
- A WARC larger than RAM is fine for `warcio` (streaming), but materialising to a single
  filesystem with 250-byte name limits will not hold an unbounded URL space — that is
  what `_hash/` is for.
