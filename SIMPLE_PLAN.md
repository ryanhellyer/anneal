# SIMPLE_PLAN.md

## Shape

1. **Spider** finds a link, inserts its target if the row is new.
2. **Fetch** requests it, following redirects, and completes the row.
3. **Place** decides the file name from the `Content-Type` and writes the body
   into `public/`.
4. **Record** the name, HTTP status and headers back into the row.
5. **Rewrite** (always, and only at the end): resolve every link against the
   files on disk and point it at the file that actually serves it.

Two artefacts: `site.db` is the archive, `public/` is a view of it. Bodies are
files because the tree is the product; everything else about a response —
headers, status, timestamps, where the body went — is a row.

Each site is one directory:

```
example.com/
  site.db   the archive
  public/   the tree nginx serves
```

That is the whole layout, so a site can be copied, moved, served or deleted as a
unit. `--db` renames the database and `--site` moves the tree, if you want them
somewhere else.

One binary, `anneal`. Crawling and rewriting run at different times, so they are
separate commands:

```
anneal crawl --seed URL   # repeatable; spider, fetch, place, record
anneal rewrite            # the pass over public/, reading site.db for page URLs
```

`--seed` is repeatable, for entry points the homepage does not link — a section
root, an unlinked index, a sitemap. A seed is inserted and fetched exactly like a
discovered link; nothing treats it specially afterwards. All seeds must share an
origin, which the first one fixes, because every target is stored without it.

The origin is not in the database. It comes from `--seed` on every run, including
a resumed one: the rows hold targets like `/about`, and the seed supplies the host
to put in front of them.

The other flags are `--exclude SUBSTR` (repeatable),
`--max-queries-per-path N` (1,000), `--jobs N`, `--delay MS`, `--timeout SEC`,
`--user-agent STR`, `--max-urls N`, `--clean` and `--no-robots`.

It obeys `robots.txt` unless `--no-robots`, sends one request per host no faster
than `--delay`, and stops adding work at `--max-urls`. A request is tried three
times with backoff; if it still fails the row is marked
`ignored = 'unreachable'` and is not retried on the next run.

A re-run never deletes anything: a target fetched again overwrites its file.
`--clean` is the reset — it deletes `site.db` (and the `-wal`/`-shm` files beside
it) and empties `public/` before starting, so the run begins with no memory of the
last one. Without it the database decides: rows still pending are fetched,
completed rows are left alone.

Orphans cannot accumulate, because the writes have one order: the row is inserted
at discovery, the body is written next, `file` and `status` are recorded last. A
crash loses at most the body, never the row, so the next run fetches the target
again and writes the same path. No file exists without a row, and no row claims a
file it does not have.

## 1. The database

One SQLite file per site, `site.db`, holding one table. Bodies stay on
disk in `public/`; the database holds what `public/` cannot: which paths were
discovered, what came back, where the body went, and what headers came with it.

```sql
PRAGMA journal_mode = WAL;

CREATE TABLE urls (
  target       TEXT PRIMARY KEY,     -- '/index.php?id=42', exactly as sent
  response_target TEXT,              -- set only when a redirect moved it
  status       INTEGER,
  content_type TEXT,
  headers      TEXT,                 -- JSON: [["Name","value"], …]
  bytes        INTEGER,              -- size actually written, for checking
  file         TEXT,                 -- where the body went; NULL until fetched
  ignored      TEXT,                 -- reason it will never be stored
  fetched_at   TEXT
);

CREATE INDEX urls_pending ON urls(target)
  WHERE status IS NULL AND ignored IS NULL;
```

The crawl is three statements:

```sql
-- discovery, deduped by the unique index
INSERT INTO urls (target) VALUES (?)
  ON CONFLICT (target) DO NOTHING;

-- the response, whatever the verdict; `ignored` says why nothing was placed
UPDATE urls SET status=?, content_type=?, headers=?, bytes=?,
                file=?, response_target=?, ignored=?, fetched_at=?
 WHERE target=?;

-- resume: discovered but never completed, so the frontier survives a crash
SELECT target FROM urls WHERE status IS NULL AND ignored IS NULL;
```

`status` is recorded even when the response is dropped: a 404 stays a 404, with
`ignored = 'status'` beside it. `ignored` is one of `exclude`, `query-cap`,
`status`, `empty`, `conflict`, `unreachable`, or `NULL` when the body is in
`public/`.

Targets dropped before the request is made for being off-host or non-HTTP get no
row at all: there is nothing to resume and nothing to report. Everything else
keeps one, `--exclude` and the query cap included, so a second run does not
rediscover it — which makes an exclusion sticky. Removing the flag does not
un-exclude anything already recorded; delete those rows, or start over with
`--clean`.

A redirect records where it ended in `response_target`, but the body belongs to
the target that was asked for, because that is the URL the page linked to. If the
destination is linked as well it gets its own row and its own file, so the bytes
are stored twice — the same trade `/` and `/index.php` already make.

### The headers must describe the file

The `headers` column holds the response headers, and the body on disk is always
plain bytes — nginx would happily serve compressed bytes from a file named
`.css`, and the page would render as garbage. So anything the server compresses
is decoded before it is written.

That makes two of the captured headers false about the file. `Content-Encoding`
says `gzip` when the file is not compressed. `Content-Length` is the length on
the wire, which is not the length on disk. Both are dropped, and `bytes` records
the size that was actually written.

Everything else is kept verbatim, so every header in the column is a true
statement about the body beside it. That matters for the later version that
serves them: replaying `Content-Encoding: gzip` over a decoded file breaks the
response, and a stale `Content-Length` truncates it. A missing header is
invisible; a wrong one is a bug you find months later.

The trap underneath is a client that compresses for you. In Go, `http.Client`
adds `Accept-Encoding: gzip` on its own, decompresses the response, and
**deletes** the `Content-Encoding` header before the caller sees it — so a naive
capture already holds a decoded body with no encoding header, or, worse, one from
a server that compressed regardless. `Transport.DisableCompression` turns that
off; any HTTP client that decompresses transparently has the same switch, and it
has to be off here.

### Which URLs need rewriting

The file that holds a response has to carry the response's real extension,
because that extension is the only thing telling nginx — and the browser — what
the content is. A `/style.php` that returns a stylesheet has to end up in a file
ending `.css`, or it is served as `text/plain` and the page renders unstyled. A
`/data` returning JSON has to end in `.json` for the same reason.

So the name changes. And once the name changes, the URL no longer reaches it:
every link pointing at that URL has to be rewritten to point at the file.

Measured against §4's config, only three shapes are fine as they stand — the URL
finds its file with no rewriting:

| URL | file | status |
|---|---|---|
| `/` | `index.html` | 200 |
| `/bla`, `/bla/` | `bla/index.html` | 200 |
| `/css/site.css` | `css/site.css` | 200 |

Everything else 404s, because the file is not at that path:

| URL | Content-Type | file | link is rewritten to |
|---|---|---|---|
| `/index.php` | text/html | `index.php.html` | `/index.php.html` |
| `/style.php` | text/css | `style.php.css` | `/style.php.css` |
| `/data` | application/json | `data/index.json` | `/data/index.json` |
| `/index.php?id=42` | text/html | `index.php.2a553fd53e66b69f.html` | `/index.php.2a553fd53e66b69f.html` |

`/index.php` is stored separately from `/`, so a PHP homepage ends up as two
files with the same bytes — the price of every URL keeping its own name. Every
rename is recorded in `urls.file`, which the rewrite reads for one thing only:
which URL each page answers.

The files a rewrite is applied to are the ones that can contain a link —
`.html`, `.css`, `.js`, `.json`, `.xml`, `.svg`. Images, fonts and other binaries
are never opened.

## 2. Keep or drop

| condition | verdict |
|---|---|
| scheme is not `http`/`https` — `mailto:`, `javascript:`, `data:`, `ftp:`, `tel:` | drop, no row |
| different host from the origin | drop, no row |
| matches `--exclude` | drop, row `ignored = 'exclude'` |
| more than `--max-queries-per-path` (1,000) distinct queries on one path | drop, row `ignored = 'query-cap'` |
| status is not 200 after following redirects | row `ignored = 'status'` |
| body is empty | row `ignored = 'empty'` |
| everything else | placed in `public/` |

### What counts as a link

One inventory, used by both the spider and the rewrite: every URL found in these
places is discovered, and rewritten in the same place.

| where | what |
|---|---|
| `<a>`, `<area>`, `<link>` | `href` |
| `<script>`, `<img>`, `<iframe>`, `<frame>`, `<embed>`, `<source>`, `<track>`, `<audio>`, `<video>`, `<input type="image">` | `src` |
| `<img>`, `<source>` | `srcset`, `data-srcset` |
| `<link rel="preload">` | `imagesrcset` |
| `<video>` | `poster` |
| `<object>` | `data` |
| lazy-loading markup | `data-src`, `data-original` |
| `<meta http-equiv="refresh">` | the URL in `content` |
| `style` attribute, `<style>` element, `.css` | `url(…)` and `@import "…"` |
| `sitemap.xml` | `<loc>` |
| `.js`, `.json` | a string literal that is entirely a URL |

Two deliberate omissions: `<form action>`, because a POST target is not a static
file, and any URL assembled at runtime — `fetch('/api?x=' + v)`, a template
literal — because it does not exist as text until the browser builds it.

Fragments are stripped before anything else happens: `#top` and `/page#top` both
mean `/page`, and a fragment-only link is not a target at all. An empty query
(`/x?`) is the same target as `/x`.

`--exclude` is the manual escape hatch: a repeatable substring matched anywhere
in the target, for traps the crawler cannot recognise — `--exclude date=`,
`--exclude /calendar/`, `--exclude /search`. Substrings rather than globs or
regexes, because `?` and `*` both appear in real URLs and would make any pattern
syntax ambiguous at exactly the point it matters.

It is applied at discovery, before the query cap, so a path excluded for its
parameters never consumes its quota.

## 3. Where a URL lands

The extension comes from the **response's `Content-Type`**, not from the URL.
That is the whole trick: if the file on disk has the right extension, nginx
serves the right MIME type and nothing has to run.

**Content-Type to extension.** Strip everything from the first `;`, so
`text/html; charset=UTF-8` is `text/html`, and lowercase what is left. The
extension is the first of:

1. the preferred extension below;
2. the first extension `nginx/mime.types` lists for that type — file order, which
   is nginx's own idea of canonical;
3. `.bin`.

| type | ext | type | ext |
|---|---|---|---|
| text/html | html | image/svg+xml | svg |
| text/css | css | image/jpeg | jpg |
| application/javascript, text/javascript | js | image/png | png |
| application/json | json | image/gif | gif |
| text/plain | txt | image/webp | webp |
| text/xml, application/xml | xml | image/avif | avif |
| application/xhtml+xml | xhtml | image/x-icon | ico |
| text/csv | csv | font/woff2 | woff2 |
| text/markdown | md | font/woff | woff |
| application/rss+xml | rss | font/ttf | ttf |
| application/atom+xml | atom | font/otf | otf |
| application/pdf | pdf | audio/mpeg | mp3 |
| application/wasm | wasm | video/mp4 | mp4 |
| application/zip | zip | video/webm | webm |
| application/octet-stream | bin | | |

The rule cannot be "the shortest extension", which is what a reverse map suggests:
`nginx/mime.types` lists `text/html` as `htm html shtml` and `application/json` as
`json map`, so the shortest would give `index.htm` and `data/index.map`. The table
pins the types a site is actually made of and leaves the rest to nginx's order.

Two rows list a pair rather than one type, because servers disagree about which
name to send: `application/javascript`/`text/javascript` and
`text/xml`/`application/xml`. Without that, a `.js` served as `text/javascript`
would be kept as `app.js.es` — the one extension the map has for that type.

A type with no extension at all — including a missing `Content-Type` — gets
`.bin`, and §4 sets `default_type application/octet-stream` so those download
rather than render as `text/plain`.

**The path.** With the extension known, the file name follows four rules:

1. Ends in `/` → `index.<ext>` in that directory.
2. Last segment's extension maps to the same `Content-Type` in `nginx/mime.types`
   → natural path.
3. No extension → `index.<ext>` inside a directory named after the segment.
4. An extension that doesn't match → the segment with the real extension
   appended: `/theme.php` → `theme.php.css`, `/index.php` → `index.php.html`.

"Maps to the same type" is the test that matters, because it is nginx that serves
the file: an extension is kept only when nginx's own table turns it back into the
type the origin sent. The comparison is case-insensitive — nginx lowercases the
extension before looking it up (measured) — so `/LOGO.PNG` keeps its spelling and
still serves as `image/png`.

| URL | Content-Type | file | rule |
|---|---|---|---|
| `/` | text/html | `index.html` | 1 |
| `/bla` | text/html | `bla/index.html` | 3 |
| `/bla/` | text/html | `bla/index.html` | 1 |
| `/bla/team` | text/html | `bla/team/index.html` | 3 |
| `/index.php` | text/html | `index.php.html` | 4 |
| `/index.php` | text/css | `index.php.css` | 4 |
| `/bla/index.php` | text/html | `bla/index.php.html` | 4 |
| `/css/site.css` | text/css | `css/site.css` | 2 |
| `/img/logo.png` | image/png | `img/logo.png` | 2 |
| `/theme.php` | text/css | `theme.php.css` | 4 |
| `/style.css` | text/html | `style.css.html` | 4 |
| `/data` | application/json | `data/index.json` | 3 |

Rules 3 and 4 are what keep a page and a sibling URL apart: `/bla` becomes a
directory and cannot collide with `/bla.html`, while `/theme.php` keeps its name
and only gains an extension, because a real `/theme.php.css` is not a URL anyone
publishes.

The one overlap rule 4 can hit is a URL whose own name is the appended one:
`/style.css` returning HTML becomes `style.css.html`, which is also where
`/style.css.html` would live. First row written wins and the loser keeps its row
with `ignored = 'conflict'`, so the overlap is visible rather than silent. The
directory form would avoid that and collide with `/style.css/…` instead, which is
no cheaper.

**Names keep the raw target.** Percent-encoding is never decoded, so
`/%f0%9f%98%84` becomes a directory whose name is literally `%f0%9f%98%84`. A
link to that file therefore has to encode the `%` again — the URL is
`/%25f0%259f%2598%2584/index.html` — which the rewrite does by escaping the file
path once more on the way out.

Every resolved path is checked to stay inside `public/` before anything is written;
anything that escapes is refused and logged.

### The hash

Wherever this plan says "hash", it means one specific thing:

> SHA-256 over the UTF-8 bytes of the raw string — query text, path segment, or
> whole target — rendered as **lowercase hex** and truncated to its **first 16
> characters**.

All three parts matter. Hex rather than base64, because `+` and `/` in a base64
digest are a filename hazard and a path separator respectively. Lowercase,
because `A` and `a` are different files on ext4. Sixteen characters, because the
filename has to stay readable and 64 bits is far more than this needs: a
20M-page crawl has roughly a one-in-100,000 chance of any collision, and a
collision is not silent — the second target becomes `ignored = 'conflict'`.

SHA-256 rather than a faster xxhash because the input is a few dozen bytes, where
the difference is measured in nanoseconds, and because it is in every standard
library — the name of a file must not depend on which language wrote it.

### When a name is too long

Filesystem limits are 255 bytes per path component and about 4000 for the whole
path, and percent-encoding eats into that — a 90-byte UTF-8 segment becomes 270
characters. Anything over is hashed rather than truncated, and the rule is the
same for every component; a long query is not special, only the most common:

| too long | becomes |
|---|---|
| the leaf, over 255 bytes | `sha256(leaf)[:16].<ext>` |
| a directory component, over 255 bytes | `sha256(component)[:16]` |
| the encoded path, over 4000 bytes | `sha256(target)[:16].<ext>`, in the root |

A query already ends in a fixed 16 characters, so it cannot overflow on its own;
what is left is a long path. A hashed directory keeps its depth, so
`/a/<long slug>/` becomes `a/<hash>/index.html` rather than collapsing to the
root. The row records the mapping, so nothing has to reverse it.

### Query URLs

`?page=2` is a real page; `?v=3` on a stylesheet is decoration. Both are kept,
but the query is hashed into the name instead of written into it.

**Name.**

| URL | Content-Type | file |
|---|---|---|
| `/bla?page=2` | text/html | `bla.bc7c7eb0ad0afdec.html` |
| `/bla/?page=2` | text/html | `bla/index.bc7c7eb0ad0afdec.html` |
| `/index.php?id=42` | text/html | `index.php.2a553fd53e66b69f.html` |
| `/index.php?id=43` | text/css | `index.php.5dc6f16a89ab7ab6.css` |
| `/bla/index.php?id=42` | text/html | `bla/index.php.2a553fd53e66b69f.html` |
| `/css/site.css?v=3` | text/css | `css/site.css.951eba4f02508cd8.css` |
| `/app.js?t=1699` | application/javascript | `app.js.070f2f0b177634f3.js` |

`?` cannot be used on Windows, survives neither zip nor a USB stick without
damage, and is a glob character in every shell — `rm bla?page=2.html` deletes the
wrong file. Writing the query into the name also forces the `/` escaping and the
`%252F` double encoding into every rewritten link. Hashing avoids all of it: the
path, `sha256` of the raw query text, and the real extension.

The path is kept, so a query page still sits next to the thing it queries, and a
target ending in `/` uses `index` as the base. Rewritten links are therefore
plain paths — `/index.php.2a553fd53e66b69f.html` — with no percent-encoding at
all.

Nothing collapses: `?v=1` and `?v=2` on one stylesheet become two files. Where
the bytes match that is a duplicate, and where they differ it is the point — a
cache-buster that actually busts.

**Rewrite runs at the end, not during the download.**

The extension is only known once the response arrives, so a link cannot be
rewritten at discovery, and it cannot be rewritten when its target is downloaded
either — the link lives in a page that was written minutes earlier, and the page
linking to it may be fetched after it. There is no moment during the crawl when a
link and its target are both known.

So rewriting is a separate pass, after the fetch phase. Filenames are derived from
targets, so for every link the file on disk is the answer; the one thing it
cannot answer is which URL a file was reached at.

The pass walks `public/` — `.html`, `.css`, `.js`, `.json`, `.xml`, `.svg` — parses
each file, resolves every link against the page's own URL, and looks up the
target on disk. That URL comes from the row whose `file` is this path: a relative
link in `bla/index.html` means `/x` if the page was reached at `/bla`, and
`/bla/x` if at `/bla/`.

When several rows share the file, the trailing-slash one wins — `/bla/` is the
form a directory index is published at. With no database beside the tree, assume
that form for every directory.

| target | look for |
|---|---|
| `/css/site.css` (honest extension) | `css/site.css` itself |
| `/data` (no extension) | `data/index.<ext>` |
| `/style.php` (lying extension) | `style.php.<ext>` |
| `/index.php?id=42` | `index.php.2a553fd53e66b69f.<ext>` |

The `<ext>` is the real extension, decided when the file was written. A query adds
one more thing in front of it: the 16 hex characters of the hash. That is what
keeps the two shapes apart — `data/index.json` is `/data/`, while
`data/index.2a553fd53e66b69f.json` is `/data/?id=42` — so a plain target matches
`index.<ext>` with no hash, and a query target matches `<base>.<hash>.<ext>` and
nothing else.

Found where the plain config already serves that target, and nothing changes: the
file is the target without its leading slash, or the target with `index.html`
under it. So `/css/site.css` is its own file, and `/`, `/bla` and `/bla/` are
already `index.html` and `bla/index.html`. Found anywhere else, and the link is
rewritten to `/<file>`. Not found at all, and it is left alone, because the URL
was never stored and would 404 either way.

Directory listings are read once and cached, so this is one `readdir` per
directory, not a `stat` per extension. With 1574 extensions in `nginx/mime.types`,
iterating the list per link would be billions of syscalls.

A target with a query is looked up by computing the name it must have — base,
`.`, `sha256(query)[:16]`, `.` — and matching that prefix, the extension being
whatever is on disk. For an extensionless target, never look for `x.*`: `x.html`
belongs to `/x.html`, and `/x` is served from `x/index.html`.

The filesystem cannot resolve everything: `/x` and `/x/` both land on
`x/index.html`, so an `index.json` beside it would never be served at all. That is
why placement refuses the second one as a conflict (§4) rather than writing two
files, and why a row's `file` wins when it exists.

That also means:

- the pass is exact rather than predictive: `/style.php` is rewritten because it
  really did land in `style.php.css`, not because its name looked odd;
- running it twice is harmless, because a rewritten link points at `/<file>`,
  which resolves to itself and is therefore not moved;
- it can be re-run after the database is gone, or after files are moved by hand.

The database never holds a filename anywhere except the one row it belongs to,
so there is nothing to string-replace. The text to edit is the file on disk.

Links are emitted root-absolute, because the file for `/bla?page=2` sits beside
the `bla/` directory rather than inside it, so a relative link would have to
climb out (`../bla.bc7c7eb0ad0afdec.html`). Root-absolute assumes the tree is
served at the root, which §4 does.

**Where it applies.** Same-origin literals, in the places §2 inventories, and only
inside the attribute, the `url()`, or the XML text they belong to — never a regex
over the whole document:

| where | becomes |
|---|---|
| `href`, `src`, `poster`, `data` | `/index.php?id=42` → `/index.php.2a553fd53e66b69f.html` |
| `srcset`, `data-srcset`, `imagesrcset` | each candidate URL, resolved the same way |
| `data-src`, `data-original` | the same as `src` |
| `<meta http-equiv="refresh" content="0;url=…">` | the URL part |
| CSS `url()`, `@import "…"` | `bg.php?v=3` → `/bg.php.951eba4f02508cd8.css` |
| `<loc>` in `sitemap.xml` | rewritten the same way |
| a quoted string in `.js` or `.json` | only when the whole literal is a URL |

A fragment survives the rewrite: `/page#top` is discovered as `/page` and comes
back as `/page.html#top`.

`<link rel="canonical">` is the one URL discovered and never rewritten, because it
names the original page rather than a file here — on a mirror it points at a
moved URL either way. `<form action>` is neither discovered nor rewritten: a POST
target is not a static file, and rewriting it turns a working form into a 404.

In `.js` and `.json`, a URL is replaced only when the entire string literal is
one — `"/index.php?id=42"` or `'/index.php?id=42'`. Inside a concatenation, a
template literal, or a comment it is left alone, because that is where a
rewriter breaks working code.

A file that is not valid in its declared charset is skipped and logged rather
than rewritten. The pass decodes with the `charset` from the response's
`Content-Type` when there is one, and refuses when that charset is unknown.

**What it cannot fix.** A query built at runtime — `<form method="get">`,
`fetch('/api?x=' + v)`, anything assembled in JavaScript — has no literal to
rewrite. The browser builds it after load and still asks for `/api?x=…`, which
404s. Keeping those working needs a server that maps the original URL to the
file (an nginx `map`), which is not portable, or dropping the feature.

So pagination, article ids and `?lang=` switches work. Search and filter forms
do not, and no amount of crawling changes that.

## 4. Serving

```nginx
events {}

http {
    include /srv/nginx/mime.types;   # the complete map from this repo
    types_hash_max_size 4096;
    default_type application/octet-stream;

    server {
        root /srv/example.com/public;
        index index.html;

        location / {
            try_files $uri/index.html $uri =404;
        }
    }
}
```

That is the entire configuration, modules aside.

The `$uri/index.html` must come first. With the obvious `try_files $uri $uri/ =404`,
nginx 301-redirects `/bla` to `/bla/` (measured), so the URL the spider recorded
is no longer the URL that serves it. Probing the index file first returns 200 at
`/bla` itself:

| request | `$uri $uri/ =404` | `$uri/index.html $uri =404` |
|---|---|---|
| `/bla` | 301 → `/bla/` | 200 |
| `/bla/` | 200 | 200 |
| `/css/site.css` | 200 | 200 |

A directory answers only one of `/x` and `/x/`: both resolve to
`<x>/index.html`, so when the two URLs returned different types the second file
is unreachable no matter what the row says. Placement therefore treats it as a
conflict (`ignored = 'conflict'`) rather than pretending the database fixes it.

What it cannot do is serve a moved URL at its original path. `/index.php`,
`/style.php` and `/data` are 404 against this config (measured), and no
`try_files` line fixes them: the file is not at that path, and the only index the
config knows how to find inside a directory is `index.html`. That is what the
rewrite pass is for — after it, no page in the tree links to a moved URL. A
bookmarked or externally linked `/index.php` still 404s, which is the same trade
queries make.

The include is the complete `nginx/mime.types` from this repo. Stock nginx has no
mapping for a lot of what real sites serve, and an unmapped extension falls back
to `default_type`.

## 5. Open

- A kept query can have side effects: `?delete=1`, `?logout`, `?add_to_cart=`. The
  only guard is `--exclude`, which requires knowing in advance. Add a
  `--no-query-urls` switch for sites where that matters?
- Store the body of 404 and 410 responses too? §2 drops them, but an error page
  is part of how a site behaves, and the cost of keeping it is one small file.
- For fetched-but-ignored URLs, keep the body in a `cache/<sha256>` file too, or
  only the headers? Headers alone still leave the complex version re-crawling
  for content it could have kept.
