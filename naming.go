package main

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// The naming rule (SPIDER.md §7, PLAN.md §1):
//
//	path = root + <pathname> + <search> + "." + <ext>   # ordinary URL
//	path = root + <pathname> + "_index." + <ext>        # URL ends in "/"
//
// The filename is the raw request URI plus the true extension. No cleaning,
// no re-encoding, no filepath.Join.

// pathAndQuery strips scheme and authority from an absolute URL by string
// slicing, preserving every byte of the path and query.
func pathAndQuery(target string) string {
	i := strings.Index(target, "://")
	if i < 0 {
		if target == "" {
			return "/"
		}
		if target[0] != '/' {
			return "/" + target
		}
		return target
	}
	rest := target[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "/"
	}
	return rest[j:]
}

// authorityOf returns the host[:port] of an absolute URL, lowercased.
func authorityOf(target string) string {
	i := strings.Index(target, "://")
	if i < 0 {
		return ""
	}
	rest := target[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return strings.ToLower(rest)
}

// sameURL reports whether a redirect target points at the same request URI
// (a no-op redirect), comparing only path and query.
func sameURL(requestURI, location string) bool {
	if location == requestURI {
		return true
	}
	return pathAndQuery(location) == requestURI
}

// simpleName applies the naming rule to a request URI and returns the path
// relative to the output root, with a leading "/".
func simpleName(requestURI, ext string) string {
	pathname, search := splitQuery(requestURI)
	if search == "" && strings.HasSuffix(pathname, "/") {
		return pathname + "_index." + ext
	}
	return pathname + search + "." + ext
}

// splitQuery splits at the first '?'. The query keeps its leading '?'.
func splitQuery(requestURI string) (pathname, search string) {
	if i := strings.IndexByte(requestURI, '?'); i >= 0 {
		return requestURI[:i], requestURI[i:]
	}
	return requestURI, ""
}

// resolveDest turns a request URI and true extension into a destination path
// and the URL-relative name used in extra.map. Long URLs are hashed.
func resolveDest(root, requestURI, ext string) (dest, name string, hashed bool) {
	if len(requestURI) >= 250 {
		sum := sha1.Sum([]byte(requestURI))
		name = "/_hash/" + hex.EncodeToString(sum[:])[:16] + "." + ext
		return joinRoot(root, name), name, true
	}
	name = simpleName(requestURI, ext)
	return joinRoot(root, name), name, false
}

// joinRoot concatenates root and a name that starts with "/". It never cleans
// the name (which would eat trailing slashes and double slashes).
func joinRoot(root, name string) string {
	r := root
	if r == "" {
		r = "."
	}
	for len(r) > 1 && strings.HasSuffix(r, "/") {
		r = r[:len(r)-1]
	}
	if r == "/" {
		return name
	}
	return r + name
}

// dirOf is a hand-written dirname that does not clean the path
// (filepath.Dir would strip a trailing slash).
func dirOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "."
	}
	if i == 0 {
		return "/"
	}
	return p[:i]
}

// safePath rejects request URIs that could escape the output root.
func safePath(requestURI string) bool {
	if requestURI == "" || requestURI[0] != '/' {
		return false
	}
	if strings.IndexByte(requestURI, 0) >= 0 {
		return false
	}
	pathname, _ := splitQuery(requestURI)
	for _, seg := range strings.Split(pathname, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// apparentExt is the extension the URL appears to have, matching the nginx
// map regex: alphanumerics (1-20) immediately before an optional query.
func apparentExt(requestURI string) string {
	pathname, _ := splitQuery(requestURI)
	base := pathname
	if i := strings.LastIndexByte(pathname, '/'); i >= 0 {
		base = pathname[i+1:]
	}
	i := strings.LastIndexByte(base, '.')
	if i < 0 {
		return ""
	}
	e := base[i+1:]
	if e == "" || len(e) > 20 {
		return ""
	}
	for _, c := range e {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return ""
		}
	}
	return strings.ToLower(e)
}
