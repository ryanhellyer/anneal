# SIMPLE_PLAN.md

## Shape

1. **Spider** finds a link, inserts its target if the row is new.
2. **Fetch** requests it, following redirects, and completes the row.
3. **Place** decides the file name from the `Content-Type` and writes the body
   into `site/`.
4. **Record** the name, HTTP status and headers back into the row.
5. **Rewrite** (always, and only at the end): resolve every link against the
   files on disk and point it at the file that actually serves it.

Two artefacts: `example.com.db` is the archive, `site/` is a view of it. Bodies
are files because the tree is the product; everything else about a response —
headers, status, timestamps, where the body went — is a row.

## 1. The database

One SQLite file per origin, `example.com.db`, holding one table. Bodies stay on
disk in `site/`; the database holds what `site/` cannot: which paths were
discovered, what came back, where the body went, and what headers came with it.

```sql
PRAGMA journal_mode = WAL;

CREATE TABLE urls (
  id           INTEGER PRIMARY KEY,
  target       TEXT NOT NULL,        -- '/index.php?id=42', exactly as sent
  response_target TEXT,              -- set only when a redirect moved it
  status       INTEGER,
  content_type TEXT,
  headers      TEXT,                 -- JSON: [["Name","value"], …]
  bytes        INTEGER,              -- size actually written, for checking
  file         TEXT,                 -- where the body went; NULL until fetched
  ignored      TEXT,                 -- reason it will never be stored
  fetched_at   TEXT,
  UNIQUE (target)
);

CREATE INDEX urls_pending ON urls(id)
  WHERE status IS NULL AND ignored IS NULL;
```

The whole crawl is three statements:

```sql
-- discovery, deduped by the unique index
INSERT INTO urls (target) VALUES (?)
  ON CONFLICT (target) DO NOTHING;

-- fetch: one row, updated in place
UPDATE urls SET status=?, content_type=?, headers=?,
                bytes=?, file=?, fetched_at=? WHERE id=?;

-- resume: discovered but never completed, so the frontier survives a crash
SELECT target FROM urls WHERE status IS NULL AND ignored IS NULL;
```

A path that is dropped still gets a row, with `ignored` set to the reason, so a
second run does not rediscover it.

### Which URLs need rewriting

The file that holds a response has to carry the response's real extension,
because that extension is the only thing telling nginx — and the browser — what
the content is. A `/style.php` that returns a stylesheet has to end up in a file
ending `.css`, or it is served as `text/plain` and the page renders unstyled. A
`/data` returning JSON has to end in `.json` for the same reason.

So the name changes. And once the name changes, the URL no longer reaches it:
every link pointing at that URL has to be rewritten to point at the file.

Measured against §5's config, only three shapes are fine as they stand — the URL
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
rename is recorded in `urls.file`, which is what the rewrite resolves a link
against.

The files a rewrite is applied to are the ones that can contain a link —
`.html`, `.css`, `.js`, `.json`, `.xml`, `.svg`. Images, fonts and other binaries
are never opened.

## 2. Keep or drop

| condition | verdict |
|---|---|
| link is `mailto:`, `javascript:`, `data:` | drop |
| different host from the origin | drop |
| status is not 200 after redirects | drop |
| body is empty | drop |
| more than `--max-queries-per-path` (defaults to 1,000) distinct queries on one path | drop the rest |
| query strings | keep but gets it's own file name (see §3) |
| .php files | keep but gets a new file name (see §3) |
| everything else | keep |

A dropped target still gets a row, with `ignored` set to the reason, so a second
run does not rediscover it.

`--exclude` is the manual escape hatch: a repeatable substring matched anywhere
in the target, for traps the crawler cannot recognise — `--exclude date=`,
`--exclude /calendar/`, `--exclude /search`. Substrings rather than globs or
regexes, because `?` and `*` both appear in real URLs and would make any pattern
syntax ambiguous at exactly the point it matters.

## 3. Where a URL lands

The extension comes from the **response's `Content-Type`**, not from the URL.
That is the whole trick: if the file on disk has the right extension, nginx
serves the right MIME type and nothing has to run.

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
path, `sha256(query)[:16]`, and the real extension.

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

The pass walks `site/` — `.html`, `.css`, `.js`, `.json`, `.xml`, `.svg` — parses
each file, resolves every link against that file's own path, and looks up the
target on disk:

| target | look for |
|---|---|
| `/css/site.css` (honest extension) | `css/site.css` itself |
| `/data` (no extension) | `data/index.*` |
| `/style.php` (lying extension) | `style.php.*` |
| `/index.php?id=42` | `index.php.2a553fd53e66b69f.*` |

Found and already at the target's own path — `/css/site.css` in `css/site.css` —
and nothing changes. Found somewhere else, and the link is rewritten to `/<file>`.
Not found at all, and it is left alone, because the URL was never stored and
would 404 either way.

Directory listings are read once and cached, so this is one `readdir` per
directory, not a `stat` per extension. With 1574 extensions in `nginx/mime.types`,
iterating the list per link would be billions of syscalls.

A target with a query is looked up by computing the same hash — base, `.`,
`sha256(query)[:16]` — and prefix-matching that, which cannot collide with
anything. For an extensionless target, never prefix-match: `x.` would match
`x.html`, which belongs to `/x.html`, and `/x` is served from `x/index.html`.

The filesystem cannot resolve everything. A directory holding both `index.html`
and `index.json` serves two different URLs (`/x` and `/x/`), and only
`urls.file` knows which is which. Use that column when the row exists, the filesystem
otherwise.

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
served at the root, which §5 does.

**Where it applies.** Same-origin literals, and only inside the attribute, the
`url()`, or the XML text they belong to — never a regex over the whole document:

| where | becomes |
|---|---|
| `href`, `src`, `action` | `/index.php?id=42` → `/index.php.2a553fd53e66b69f.html` |
| same-page query | `?page=2` → `/bla.bc7c7eb0ad0afdec.html` |
| absolute, same origin | `https://example.com/bla?page=2` → `/bla.bc7c7eb0ad0afdec.html` |
| CSS `url()` | `bg.php?v=3` → `/bg.php.951eba4f02508cd8.css` |
| `<loc>` in `sitemap.xml` | rewritten the same way |

**What it cannot fix.** A query built at runtime — `<form method="get">`,
`fetch('/api?x=' + v)`, anything assembled in JavaScript — has no literal to
rewrite. The browser builds it after load and still asks for `/api?x=…`, which
404s. Keeping those working needs a server that maps the original URL to the
file (an nginx `map`), which is not portable, or dropping the feature.

So pagination, article ids and `?lang=` switches work. Search and filter forms
do not, and no amount of crawling changes that.

## 4. The placement app

A separate process, because that is how the spider calls it: one JSON line in,
one JSON line out.

```
$ place                        # stdin: {"target":…,"status":…,"content_type":…}
{"file":"index.php.2a553fd53e66b69f.html"}   # stdout: {"file":…} or {"ignored":"…"}
```

Stateless and pure — the same input always gives the same path, so it can be a
pipe, a batch job, or an HTTP handler if it ever needs to be one. It is the same
binary as the spider (`anneal place`), so there is still nothing to install.

It runs **after** the fetch, because every rule needs the `Content-Type`. The
spider's own job is only the cheap pre-filter: link scheme and host.

`anneal rewrite` is the one step that is not a pure function of a single URL: it
reads the files and edits `site/` in place. Same binary.

## 5. Serving

```nginx
root /srv/site;
index index.html;

location / {
    try_files $uri/index.html $uri =404;
}
```

That is the entire configuration.

The `$uri/index.html` must come first. With the obvious `try_files $uri $uri/ =404`,
nginx 301-redirects `/bla` to `/bla/` (measured), so the URL the spider recorded
is no longer the URL that serves it. Probing the index file first returns 200 at
`/bla` itself:

| request | `$uri $uri/ =404` | `$uri/index.html $uri =404` |
|---|---|---|
| `/bla` | 301 → `/bla/` | 200 |
| `/bla/` | 200 | 200 |
| `/css/site.css` | 200 | 200 |

What it cannot do is serve a moved URL at its original path. `/index.php`,
`/style.php` and `/data` are 404 against this config (measured, §1), and no
`try_files` line fixes them, because the file is not at that path and its
extension is not `.html`. That is what the rewrite pass is for: after it, no page
in the tree links to a moved URL. A bookmarked or externally linked `/index.php`
still 404s, which is the same trade queries make.

Use the complete `nginx/mime.types` already in this repo. Stock nginx has no
mapping for a lot of what real sites serve, and an unmapped extension is served
as `text/plain`.

## 6. Open

- A kept query can have side effects: `?delete=1`, `?logout`, `?add_to_cart=`. The
  only guard is `--exclude`, which requires knowing in advance. Add a
  `--no-query-urls` switch for sites where that matters?
- Does the spider follow `<script src>`, CSS `url()`, and `fetch()` literals, or
  only `<a href>` and `sitemap.xml`?
- Keep stored 404 and 410 pages? They are part of how a site behaves, and the
  cost of keeping them is one small file.
- Rewrite JavaScript string literals too, when they match a known query URL?
  Safer to leave them: a wrong rewrite there is a runtime break, and an
  unrewritten one is a 404 that at least says what happened.
- For fetched-but-ignored URLs, keep the body in a `cache/<sha256>` file too, or
  only the headers? Headers alone still leave the complex version re-crawling
  for content it could have kept.
- A URL that never answered — refused, timed out — also has `status IS NULL`, so
  every resume retries it. Add a try counter and retire it after three, or leave
  it in and let the run waste a few minutes each time?
