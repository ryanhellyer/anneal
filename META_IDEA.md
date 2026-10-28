# META_IDEA.md

## The problem

Static files carry bytes, not headers. Some sites depend on the original
`Content-Type` and headers internally: `fetch()` inspecting `content-type`, CORS
for fonts, `nosniff`, `Content-Disposition`, custom `X-*`. Get them wrong and the
site breaks silently. So copy them.

## The idea

Sidecar metadata next to each file. nginx serves files without one; Go serves
files with one.

```
/x.html -> x.html        nginx, typed by extension
/x.html -> x.html.meta   Go, with the recorded headers
```

## Routing

```nginx
location ~ \.meta$ { return 404; }        # else /x.html.meta is downloadable

location / {
    if (-f $document_root$request_uri.meta) { return 418; }
    error_page 418 = @go;
    try_files $request_uri @go;
}

location @go { proxy_pass http://127.0.0.1:9000; }
```

`$request_uri`, not `$uri` (raw path plus query). `if` + `return` is the safe
form of `if`. Unresolvable URLs fall through to Go too, so Go serves 404s. Go
down → only the meta URLs fail.

## The .meta

```json
{
  "url": "/x.html",
  "file": "x.html",
  "status": 200,
  "content_type": "text/html; charset=UTF-8",
  "headers": [["Cache-Control", "max-age=3600"]],
  "bytes": 1234,
  "sha256": "…"
}
```

`headers` is a list, not an object — `Link` and `Set-Cookie` repeat.

Never replay hop-by-hop headers (`Connection`, `Transfer-Encoding`, …).
Recompute `Content-Length`. Check `Content-Encoding` before replaying it: the
crawler may have stored a decoded body. Copy the rest verbatim.

## Naming

- path ends in `/` → `<path>/___index.<true ext>`
- the URL's extension already equals the true extension → natural path
- otherwise → `___` before the basename, then `.<true ext>`

| URL | file |
|---|---|
| `/x` | `___x.html` |
| `/x.html` | `x.html` |
| `/x/` | `x/___index.html` |
| `/plain.css` | `plain.css` |
| `/theme.php?v=3` (CSS) | `___theme.php?v=3.css` |

`___` is reserved: the build refuses and reports any URL containing it. The
extra name is not for typing — `/x` must be a file while `/x/` must be a
directory, so the page needs a name distinct from the directory.

## Measured against nginx 1.24

- `/x.html.meta` requested directly returns 200. It leaks; hence the deny rule.
- `/z`, where only `___z.html` exists, goes to Go because `try_files` found no
  file — **not** because nginx saw `___`. `try_files` can prepend a path but
  cannot insert `___` into a basename.

So the reserved key is a storage convention, not an nginx routing rule. What
that costs depends on where the marker sits:

| marker | `/x` | served by |
|---|---|---|
| basename | `___x.html` | Go, always |
| directory | `___/x.html` | nginx (`try_files /___$request_uri.html`) |

The directory form also lets nginx find the sidecar by plain concatenation
(`/___$request_uri.html.meta`), so "has a meta → Go" keeps working for pages.

## Build duties

Reserve and report `___` and `.meta` URLs. Write a `.meta` only when the
captured headers differ from what nginx would emit by itself. Store
`Content-Type` verbatim, parameters included. Keep headers as ordered lists.

## Open

Basename reads better in `ls`. Directory keeps pages on nginx. Pick one.
