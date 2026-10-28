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
unit. `--db` renames the database and `--site` moves the tree, when you want
either somewhere else.

One binary, `anneal`. Crawling and rewriting run at different times, so they are
separate commands:

```
anneal crawl --seed URL   # repeatable; spider, fetch, place, record
anneal rewrite             # the pass over public/
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

A re-run never deletes anything: a target fetched again overwrites its file, and
`--clean` empties `public/` before starting. Orphans cannot accumulate — a file is
written only for a target whose row exists, and a crash between the two leaves no
row, so the next run fetches the target again and rewrites the same path.

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

Targets dropped before the request is made — off-host, or a `mailto:`/`data:`
scheme — get no row at all. There is nothing to resume and nothing to report.
Everything else keeps one, so a second run does not rediscover it.

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
rename is recorded in `urls.file`; the rewrite can work from the filesystem alone
and consults that column only for the ambiguous cases.

The files a rewrite is applied to are the ones that can contain a link —
`.html`, `.css`, `.js`, `.json`, `.xml`, `.svg`. Images, fonts and other binaries
are never opened.

## 2. Keep or drop

| condition | verdict |
|---|---|
| link is `mailto:`, `javascript:`, `data:`, `ftp:` | drop, no row |
| different host from the origin | drop, no row |
| matches `--exclude` | drop, row `ignored = 'exclude'` |
| more than `--max-queries-per-path` (1,000) distinct queries on one path | drop, row `ignored = 'query-cap'` |
| status is not 200 after following redirects | row `ignored = 'status'` |
| body is empty | row `ignored = 'empty'` |
| everything else | placed in `public/` |

Discovery follows `href` on `<a>` and `<link>`, `src` on `<script>` and `<img>`,
each candidate in `srcset`, `url()` in CSS, and `sitemap.xml` — the same places
the rewrite handles. A `<form>` is not followed, and neither is a URL built in
JavaScript: the rewrite cannot fix those either, so crawling them only adds rows
that 404.

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
`text/html; charset=UTF-8` is `text/html`. Look the type up in the reverse of
`nginx/mime.types` and take the shortest extension, breaking ties alphabetically,
so the choice does not depend on map iteration order. A type with no extension —
including a missing `Content-Type` and `application/octet-stream` — gets `.bin`,
and §4 sets `default_type application/octet-stream` so those download rather than
render as `text/plain`.

1. Ends in `/` → `index.<ext>` in that directory.
2. Last segment's extension already matches the `Content-Type` → natural path.
3. No extension → `index.<ext>` inside a directory named after the segment.
4. An extension that doesn't match → the segment with the real extension
   appended: `/theme.php` → `theme.php.css`, `/index.php` → `index.php.html`.

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

So rewriting is a separate pass, after the fetch phase, and it needs no database
lookup: the filename is derived from the target, so the file itself is the
answer.

The pass walks `public/` — `.html`, `.css`, `.js`, `.json`, `.xml`, `.svg` — parses
each file, resolves every link against the page's own URL, and looks up the
target on disk. That URL comes from the row whose `file` is this path, because
`bla/index.html` may have been reached at `/bla` or at `/bla/`, and `?page=2`
resolves differently from each. Without a database, assume the trailing-slash
form.

| target | look for |
|---|---|
| `/css/site.css` (honest extension) | `css/site.css` itself |
| `/data` (no extension) | `data/index.*` |
| `/style.php` (lying extension) | `style.php.*` |
| `/index.php?id=42` | `index.php.2a553fd53e66b69f.*` |

Found where the plain config already serves that target — `file` equals the
target, or equals `<target>/index.html` — and nothing changes. That second case
is why `/bla` and `/bla/` keep working without an edit: `bla/index.html` is
already the answer. Found anywhere else, and the link is rewritten to `/<file>`.
Not found at all, and it is left alone, because the URL was never stored and
would 404 either way.

Directory listings are read once and cached, so this is one `readdir` per
directory, not a `stat` per extension. With 1574 extensions in `nginx/mime.types`,
iterating the list per link would be billions of syscalls.

A target with a query is looked up by computing the same hash — base, `.`,
`sha256(query)[:16]` — and prefix-matching that, which cannot collide with
anything. For an extensionless target, never prefix-match: `x.` would match
`x.html`, which belongs to `/x.html`, and `/x` is served from `x/index.html`.

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

**Where it applies.** Same-origin literals, and only inside the attribute, the
`url()`, or the XML text they belong to — never a regex over the whole document:

| where | becomes |
|---|---|
| `href`, `src` | `/index.php?id=42` → `/index.php.2a553fd53e66b69f.html` |
| `srcset`, `poster`, `data-src` | each candidate URL, resolved the same way |
| `<meta http-equiv="refresh" content="0;url=…">` | the URL part |
| CSS `url()` | `bg.php?v=3` → `/bg.php.951eba4f02508cd8.css` |
| `<loc>` in `sitemap.xml` | rewritten the same way |
| a quoted string in `.js` or `.json` | only when the whole literal is a URL |

Left alone on purpose: `<form action>` — a POST target is not a static file, and
rewriting it turns a working form into a 404 — and `<link rel="canonical">`,
which names the original page rather than a file here.

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
`/style.php` and `/data` are 404 against this config (measured, §1), and no
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
