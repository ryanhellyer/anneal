package main

import (
	"strings"
	"testing"
)

func TestSimpleName(t *testing.T) {
	cases := []struct{ uri, ext, want string }{
		{"/x", "html", "/x.html"},
		{"/x.html", "html", "/x.html.html"},
		{"/x/", "html", "/x/_index.html"},
		{"/x/index.html", "html", "/x/index.html.html"},
		{"/plain.css", "css", "/plain.css.css"},
		{"/index.php?bla=whatever", "html", "/index.php?bla=whatever.html"},
		{"/theme.php?v=3", "css", "/theme.php?v=3.css"},
		{"/img.png", "png", "/img.png.png"},
		{"/search?q=a&page=2", "html", "/search?q=a&page=2.html"},
		{"/", "html", "/_index.html"},
	}
	for _, c := range cases {
		if got := simpleName(c.uri, c.ext); got != c.want {
			t.Errorf("simpleName(%q,%q) = %q, want %q", c.uri, c.ext, got, c.want)
		}
	}
}

func TestPathAndQueryKeepsRawBytes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/x", "/x"},
		{"https://example.com/", "/"},
		{"https://example.com", "/"},
		{"https://example.com/a%7Eb", "/a%7Eb"},
		{"https://example.com/a%2fb", "/a%2fb"},
		{"https://example.com/p?q=%2B", "/p?q=%2B"},
		{"https://example.com:8300/x?a=b", "/x?a=b"},
	}
	for _, c := range cases {
		if got := pathAndQuery(c.in); got != c.want {
			t.Errorf("pathAndQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApparentExt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/x", ""},
		{"/x.html", "html"},
		{"/theme.php?v=3", "php"},
		{"/a.min.js", "js"},
		{"/nope.", ""},
	}
	for _, c := range cases {
		if got := apparentExt(c.in); got != c.want {
			t.Errorf("apparentExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafePath(t *testing.T) {
	unsafe := []string{"/../etc/passwd", "/a/../b", "x", "", "/a/%2e%2e/b"}
	for _, u := range unsafe {
		if u == "/a/%2e%2e/b" {
			// %2e%2e is a literal filename segment, not "..", so it is allowed.
			if !safePath(u) {
				t.Errorf("safePath(%q) = false, want true", u)
			}
			continue
		}
		if safePath(u) {
			t.Errorf("safePath(%q) = true, want false", u)
		}
	}
	if !safePath("/a/b?c=d") {
		t.Error("safePath rejected a normal query URL")
	}
}

func TestJoinRoot(t *testing.T) {
	cases := []struct{ root, name, want string }{
		{"./", "/x", "./x"},
		{".", "/x", "./x"},
		{"/tmp/site", "/x", "/tmp/site/x"},
		{"/tmp/site/", "/x", "/tmp/site/x"},
		{"/", "/x", "/x"},
	}
	for _, c := range cases {
		if got := joinRoot(c.root, c.name); got != c.want {
			t.Errorf("joinRoot(%q,%q) = %q, want %q", c.root, c.name, got, c.want)
		}
	}
}

func TestResolveDestHashesLongURLs(t *testing.T) {
	long := "/" + strings.Repeat("a", 300)
	dest, name, hashed := resolveDest("/root", long, "html")
	if !hashed {
		t.Fatal("expected long URL to be hashed")
	}
	if !strings.HasPrefix(name, "/_hash/") || !strings.HasSuffix(name, ".html") {
		t.Errorf("unexpected hash name %q", name)
	}
	if dest != "/root"+name {
		t.Errorf("dest %q, want %q", dest, "/root"+name)
	}
}

func TestExtForType(t *testing.T) {
	m := parseMimeTypes(embeddedMime)
	cases := []struct{ ct, want string }{
		{"text/html", "html"},
		{"text/html; charset=UTF-8", "html"},
		{"text/css", "css"},
		{"application/javascript", "js"},
		{"application/json", "json"},
		{"image/png", "png"},
		{"image/jpeg", "jpg"},
	}
	for _, c := range cases {
		got, ok := m.extForType(c.ct)
		if !ok || got != c.want {
			t.Errorf("extForType(%q) = %q,%v want %q", c.ct, got, ok, c.want)
		}
	}
	if _, ok := m.extForType("application/x-nonexistent-anneal"); ok {
		t.Error("extForType accepted an unknown type")
	}
	if _, ok := m.extForType(""); ok {
		t.Error("extForType accepted an empty type")
	}
}
