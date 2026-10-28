package main

import (
	_ "embed"
	"os"
	"strings"
)

// embeddedMime is the complete nginx/mime.types shipped with this repo
// (PLAN.md §2.1). It makes the binary self-contained; an external file passed
// with --mime overrides it.
//
//go:embed nginx/mime.types
var embeddedMime string

// MimeTable is an inverted nginx/mime.types: extension <-> MIME type.
type MimeTable struct {
	extToType map[string]string
	typeToExt map[string]string
}

// canonicalExt pins the extension Anneal prefers when a type has several
// spellings (image/jpeg ships "jpeg", we want "jpg").
var canonicalExt = map[string]string{
	"text/html":                 "html",
	"text/css":                  "css",
	"text/javascript":           "js",
	"application/javascript":    "js",
	"application/json":          "json",
	"application/manifest+json": "webmanifest",
	"application/ld+json":       "jsonld",
	"text/plain":                "txt",
	"text/markdown":             "md",
	"text/xml":                  "xml",
	"application/xml":           "xml",
	"image/jpeg":                "jpg",
	"image/png":                 "png",
	"image/gif":                 "gif",
	"image/webp":                "webp",
	"image/avif":                "avif",
	"image/svg+xml":             "svg",
	"image/x-icon":              "ico",
	"image/vnd.microsoft.icon":  "ico",
	"font/woff":                 "woff",
	"font/woff2":                "woff2",
	"application/pdf":           "pdf",
	"application/wasm":          "wasm",
	"application/zip":           "zip",
	"application/gzip":          "gz",
	"video/mp4":                 "mp4",
	"audio/mpeg":                "mp3",
	"application/octet-stream":  "bin",
}

func parseMimeTypes(data string) *MimeTable {
	t := &MimeTable{
		extToType: map[string]string{},
		typeToExt: map[string]string{},
	}
	i := strings.IndexByte(data, '{')
	j := strings.LastIndexByte(data, '}')
	if i < 0 || j <= i {
		return t
	}
	for _, line := range strings.Split(data[i+1:j], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSuffix(line, ";")
		fs := strings.Fields(line)
		if len(fs) < 2 {
			continue
		}
		mimeType := strings.ToLower(fs[0])
		for _, ext := range fs[1:] {
			ext = strings.ToLower(ext)
			if ext == "" {
				continue
			}
			if _, ok := t.extToType[ext]; !ok {
				t.extToType[ext] = mimeType
			}
			if _, ok := t.typeToExt[mimeType]; !ok {
				t.typeToExt[mimeType] = ext
			}
		}
	}
	return t
}

// loadMime reads an explicit --mime file, else searches the usual locations,
// else falls back to the embedded copy.
func loadMime(path string) (*MimeTable, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return parseMimeTypes(string(b)), nil
	}
	var cands []string
	if env := os.Getenv("ANNEAL_MIME"); env != "" {
		cands = append(cands, env)
	}
	cands = append(cands, "nginx/mime.types", "mime.types", "/etc/nginx/mime.types")
	for _, c := range cands {
		if b, err := os.ReadFile(c); err == nil {
			return parseMimeTypes(string(b)), nil
		}
	}
	return parseMimeTypes(embeddedMime), nil
}

// typeAlias maps non-standard Content-Type values real servers emit onto the
// registered type they mean, so materialise and verify agree on them.
var typeAlias = map[string]string{
	"image/jpg":                "image/jpeg",
	"image/pjpeg":              "image/jpeg",
	"image/x-png":              "image/png",
	"application/x-javascript": "application/javascript",
	"text/x-javascript":        "application/javascript",
	"application/ecmascript":   "application/javascript",
	"text/ecmascript":          "application/javascript",
}

// extForType returns the canonical extension for a Content-Type header value.
func (t *MimeTable) extForType(ct string) (string, bool) {
	base := normalizeType(ct)
	if base == "" {
		return "", false
	}
	if e, ok := canonicalExt[base]; ok {
		return e, true
	}
	if e, ok := t.typeToExt[base]; ok {
		return e, true
	}
	return "", false
}

// typeForExt is the reverse lookup, used by verify to check the served type.
func (t *MimeTable) typeForExt(ext string) (string, bool) {
	e, ok := t.extToType[strings.ToLower(ext)]
	return e, ok
}

func normalizeMediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// normalizeType is normalizeMediaType plus the non-standard-type aliases, so a
// type captured from the site and the type nginx serves compare equal.
func normalizeType(ct string) string {
	base := normalizeMediaType(ct)
	if alias, ok := typeAlias[base]; ok {
		return alias
	}
	return base
}
