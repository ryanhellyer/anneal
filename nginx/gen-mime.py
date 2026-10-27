import re, sys

# 1. stock nginx values win  (no behaviour change for anything already correct)
# 2. modern supplement for anything stock lacks (IANA-correct current values)
# 3. /etc/mime.types (Debian/IANA bulk list) for the long tail

SUPPLEMENT = {
 # code / data
 "mjs":"application/javascript","cjs":"application/javascript",
 "map":"application/json","jsonld":"application/ld+json",
 "geojson":"application/geo+json","topojson":"application/topo+json",
 "webmanifest":"application/manifest+json","wasm":"application/wasm",
 "graphql":"application/graphql","gql":"application/graphql",
 "mpd":"application/dash+xml","ipynb":"application/x-ipynb+json",
 "parquet":"application/vnd.apache.parquet","avro":"application/avro",
 "sqlite":"application/vnd.sqlite3","sqlite3":"application/vnd.sqlite3",
 "yaml":"application/yaml","yml":"application/yaml","toml":"application/toml",
 # text
 "md":"text/markdown","markdown":"text/markdown","csv":"text/csv",
 "tsv":"text/tab-separated-values","ics":"text/calendar","vcf":"text/vcard",
 "vtt":"text/vtt","srt":"application/x-subrip","ass":"text/x-ssa","ssa":"text/x-ssa",
 "appcache":"text/cache-manifest",
 # sources (sometimes served as-is)
 "ts":"application/typescript","tsx":"text/jsx","jsx":"text/jsx",
 "scss":"text/x-scss","sass":"text/x-sass","less":"text/x-less","styl":"text/x-stylus",
 # images
 "avif":"image/avif","avifs":"image/avif-sequence","jxl":"image/jxl",
 "heic":"image/heic","heif":"image/heif","jp2":"image/jp2","jpx":"image/jpx",
 "ktx2":"image/ktx2","basis":"image/basis","apng":"image/apng","cur":"image/x-icon",
 "psd":"image/vnd.adobe.photoshop","jfif":"image/jpeg",
 # fonts
 "ttf":"font/ttf","otf":"font/otf","ttc":"font/collection",
 "woff":"font/woff","woff2":"font/woff2",
 # audio
 "oga":"audio/ogg","opus":"audio/ogg","weba":"audio/webm",
 "flac":"audio/flac","aac":"audio/aac","wav":"audio/wav",
 # archives
 "gz":"application/gzip","tgz":"application/gzip","tar":"application/x-tar",
 "bz2":"application/x-bzip2","xz":"application/x-xz","zst":"application/zstd",
 "lz":"application/x-lzip","lz4":"application/x-lz4","br":"application/x-brotli",
 # packages / misc
 "apk":"application/vnd.android.package-archive","crx":"application/x-chrome-extension",
 "torrent":"application/x-bittorrent",
 # 3D
 "glb":"model/gltf-binary","gltf":"model/gltf+json","usdz":"model/vnd.usdz+zip",
 "stl":"model/stl","obj":"model/obj",
 # ebooks / comics
 "mobi":"application/x-mobipocket-ebook","azw":"application/vnd.amazon.ebook",
 "azw3":"application/vnd.amazon.ebook","cbz":"application/vnd.comicbook+zip",
 "cbr":"application/vnd.comicbook-rar","djvu":"image/vnd.djvu",
 # feeds
 "rss":"application/rss+xml","atom":"application/atom+xml",
}

def parse(path):
    out={}
    for line in open(path, encoding="utf-8", errors="replace"):
        line=line.split("#",1)[0].strip()
        if not line: continue
        parts=line.split()
        if len(parts)<2: continue
        mime=parts[0]
        if not re.match(r'^[A-Za-z0-9.+-]+/[A-Za-z0-9.+-]+$', mime): continue
        for e in parts[1:]:
            out.setdefault(e.rstrip(';').lower(), mime)
    return out

stock  = parse("/etc/nginx/mime.types")
system = parse("/etc/mime.types")
table, src = {}, {}
for e,m in system.items(): table[e]=m; src[e]="system"
for e,m in SUPPLEMENT.items(): table[e]=m; src[e]="supplement"
for e,m in stock.items():     table[e]=m; src[e]="stock"   # highest priority

good = {e:m for e,m in table.items()
        if re.match(r'^[A-Za-z0-9][A-Za-z0-9._+-]{0,15}$', e) and not e.isdigit()}
by_mime={}
for e,m in good.items(): by_mime.setdefault(m,[]).append(e)

lines=[]
for m in sorted(by_mime):
    exts=" ".join(sorted(by_mime[m]))
    lines.append(f"    {m:<52} {exts};")

open("/workspace/nginx/mime.types","w").write(
 "# Complete extension -> MIME map for static sites.\n"
 "# Replaces /etc/nginx/mime.types (include this INSTEAD, not alongside).\n"
 f"# {len(good)} extensions, {len(by_mime)} types.\n\n"
 "types {\n" + "\n".join(lines) + "\n}\n")

print("extensions:", len(good), " of which stock-only:", sum(1 for e in good if e not in system), " types:", len(by_mime))
lost=[(e,m) for e,m in stock.items() if e not in good]
print("stock extensions missing from output:", lost or "none")
changed=[(e,stock[e],good[e]) for e in stock if e in good and good[e]!=stock[e]]
print("stock extensions whose MIME changed:", changed or "none")
