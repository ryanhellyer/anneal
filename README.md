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
--warc PATH       copy the WARC somewhere other than <path>/nginx/site.warc.gz
--no-warc         discard the WARC
--mime PATH       use a specific nginx/mime.types
--ignore-robots   do not honour robots.txt (only on a server you own)
--wipe            delete --path first (refused if it is not a safe output root)
--scratch DIR     crawl scratch directory (default: a temp dir)
```

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

    server {
        root /srv/static/example.com;
        index _index.html;
        include /path/to/nginx/redirects.conf;   # generated
        location / {
            try_files $resolved $request_uri.$ext $request_uri.html $request_uri.css
                      $request_uri.js $request_uri.json $uri $uri/ =404;
        }
    }
}
```

## Tests

```sh
make test
```
