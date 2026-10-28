# Anneal

Anneal crawls a live website and writes it to disk in the URL -> file layout
described in `PLAN.md`, ready for the nginx configuration there. The design and
rationale are in `SPIDER.md`.

It is a single static Go binary, standard library only.

## Build

```sh
make build          # CGO_ENABLED=0 go build -trimpath -o anneal .
```

or copy it onto a build host with:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o anneal .
```

## Use

```sh
anneal --url=https://example.com --path=./
```

This crawls `example.com` with `wget` into a WARC, then materialises the WARC
into `./` (the tree goes directly into `--path`). Change `--url` to mirror a
different site and `--path` to mirror it somewhere else. The generated nginx
fragments land in `<path>/nginx/`:

| file | purpose |
|---|---|
| `extra.map` | `map $request_uri $resolved` overrides, `include`d at `http` level |
| `redirects.conf` | `301`s, `include`d inside the `server` block |
| `manifest.json` | `url -> file, ct, ext, status, bytes, sha256` |
| `report.md` | stored / skipped / mapped / hashed / unmapped types |
| `site.warc.gz` | the crawl, kept as the audit trail |

Exit codes: `0` clean, `1` error, `2` a Content-Type had no extension (add it
to `nginx/mime.types` and re-run).

### Other flags

```
--seed URL        extra seed URL (sitemap.xml, access-log URLs, ...), repeatable
--warc PATH       write the WARC somewhere other than <path>/nginx/site.warc.gz
--no-warc         discard the WARC
--recrawl         crawl again even if a WARC already exists (default: reuse it)
--mime PATH       use a specific nginx/mime.types
--ignore-robots   do not honour robots.txt (only on a server you own)
--wipe            delete --path first (refused if it is not a safe output root)
--scratch DIR     crawl scratch dir (default: <path>/.anneal-crawl, removed after)
```

Nothing is written outside `--path` unless you ask for it. The WARC is written
straight to `<path>/nginx/site.warc.gz` (no copy step); `wget`'s transient
page downloads go to `<path>/.anneal-crawl`, which is removed when the run ends.

## Re-running and getting stuck

The crawl does not resume mid-download — no recursive WARC crawler does that
well. But re-running is safe and cheap once a crawl has produced a WARC:

- **If a WARC already exists for the same origin, Anneal reuses it** and goes
  straight to materialise. So if the crawl was interrupted (or the site went
  away), run the same command again and it finishes from the WARC it already has
  instead of downloading again.
- A WARC tagged with a different origin is not reused; changing `--url` with the
  same `--path` crawls fresh rather than serving stale bytes.
- **A failed `--recrawl` never destroys the previous WARC** — the new crawl is
  written alongside and moved into place only on success.
- A WARC killed mid-record still materialises everything before the truncated
  tail (with a warning), so a stuck crawl is not a total loss.
- The crawl is bounded: `--timeout=30`, `--tries=3`, `--waitretry=5`, so a dead
  connection cannot hang it forever.

To force a completely fresh fetch, pass `--recrawl`.


### Stages

The pipeline is three separable stages, each usable on its own:

```sh
anneal crawl       --origin https://example.com --warc site
anneal materialise --warc site.warc.gz --origin https://example.com \
                   --root ./build/example.com --out ./build/example.com/nginx
anneal verify      --base http://127.0.0.1:8080 \
                   --manifest ./build/example.com/nginx/manifest.json
```

`verify` re-serves the tree with the real nginx config and compares every
manifest URL (status, body sha256, Content-Type) against the crawl. It is the
gate: exit `1` on any mismatch.

## Wiring

```nginx
http {
    include /etc/nginx/mime.full.types;   # deploy nginx/mime.types here
    types_hash_max_size 4096;

    map $request_uri $ext { "~\.(?<e>[A-Za-z0-9]{1,20})(?:\?.*)?$" $e; default ""; }
    include /path/to/nginx/extra.map;     # generated
    map_hash_max_size 8192;               # extra.map holds thousands of URIs
    map_hash_bucket_size 512;             # keys are full request URIs

    server {
        root /srv/static/example.com;
        index _index.html;
        include /path/to/nginx/redirects.conf;   # generated
        location / {
            try_files $resolved $request_uri.$ext $request_uri.html $request_uri.css
                      $request_uri.js $request_uri.json
                      ${request_uri}_index.html ${request_uri}_index.css
                      ${request_uri}_index.js ${request_uri}_index.json
                      $uri $uri/ =404;
        }
    }
}
```

The `${request_uri}_index.*` candidates serve percent-encoded directory URLs
(nginx's `$uri/` uses the decoded path). The `map_hash_*` settings are required
once `extra.map` grows past the default map-hash size.

## Tests

```sh
make test
```
