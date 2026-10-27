package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one URL in the manifest (SPIDER.md §6).
type Entry struct {
	URL    string `json:"url"`
	File   string `json:"file"`
	CT     string `json:"ct"`
	Ext    string `json:"ext"`
	Status int    `json:"status"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest is the durable record of a build.
type Manifest struct {
	Origin  string  `json:"origin"`
	Entries []Entry `json:"entries"`
}

type MaterialiseOptions struct {
	WarcPath string
	Origin   string
	Root     string
	Out      string
	Mime     *MimeTable
}

type MaterialiseResult struct {
	Stored    int
	Skipped   int
	Mapped    []string
	Redirects []string
	Hashed    []string
	Refused   []string
	OffOrigin []string
	Unmapped  map[string]int
}

// probedExts are the extensions the nginx try_files chain probes explicitly.
// A URL whose apparent extension lies needs a map entry only when its true
// extension is outside this set (SPIDER.md §7).
var probedExts = map[string]bool{"html": true, "css": true, "js": true, "json": true}

func materialise(o MaterialiseOptions) (*MaterialiseResult, error) {
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return nil, err
	}
	res := &MaterialiseResult{Unmapped: map[string]int{}}

	originHost := authorityOf(o.Origin)
	man := Manifest{Origin: o.Origin}

	mapped := map[string]string{}  // requestURI -> resolved name
	redirects := map[string]string{} // requestURI -> location
	stored := map[string]bool{}    // requestURI already written
	shaToURL := map[string][]string{}

	err := readWARC(o.WarcPath, func(rec *WARCRecord) error {
		resp, err := parseResponse(rec)
		if err != nil || resp == nil {
			return nil
		}
		target := resp.Target
		if target == "" {
			return nil
		}
		reqURI := pathAndQuery(target)

		if originHost != "" && authorityOf(target) != originHost {
			res.OffOrigin = append(res.OffOrigin, reqURI)
			res.Skipped++
			return nil
		}

		switch {
		case resp.Status >= 300 && resp.Status < 400:
			if loc := firstHeader(resp, "location"); loc != "" {
				redirects[reqURI] = loc
			}
			return nil
		case resp.Status < 200 || resp.Status >= 300:
			res.Skipped++ // not servable as a static body
			return nil
		}

		if stored[reqURI] {
			return nil // a later record for the same URL; keep the first
		}

		if !safePath(reqURI) {
			res.Refused = append(res.Refused, reqURI)
			return nil
		}

		ext, ok := o.Mime.extForType(resp.ContentType)
		if !ok {
			res.Unmapped[normalizeMediaType(resp.ContentType)]++
			return nil
		}

		dest, name, hashed := resolveDest(o.Root, reqURI, ext)
		if hashed {
			res.Hashed = append(res.Hashed, reqURI)
		}
		// Residue: the URL's apparent extension lies and the true extension is
		// not one of the probed ones, so the try_files chain cannot find it.
		if hashed || (apparentExt(reqURI) != ext && !probedExts[ext]) {
			mapped[reqURI] = name
		}

		if err := os.MkdirAll(dirOf(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, resp.Body, 0o644); err != nil {
			return err
		}

		sum := sha256.Sum256(resp.Body)
		entry := Entry{
			URL:    reqURI,
			File:   strings.TrimPrefix(name, "/"),
			CT:     resp.ContentType,
			Ext:    ext,
			Status: resp.Status,
			Bytes:  len(resp.Body),
			SHA256: hex.EncodeToString(sum[:]),
		}
		man.Entries = append(man.Entries, entry)
		stored[reqURI] = true
		shaToURL[entry.SHA256] = append(shaToURL[entry.SHA256], reqURI)
		res.Stored++
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Duplicate bodies: collapse the trailing-slash variant onto the plain one
	// (SPIDER.md §9, PLAN.md §3). A URL with a map entry is left alone.
	for _, urls := range shaToURL {
		if len(urls) < 2 {
			continue
		}
		canonical := canonicalURL(urls)
		for _, u := range urls {
			if u == canonical {
				continue
			}
			if _, isMapped := mapped[u]; isMapped {
				continue
			}
			if _, isRedir := redirects[u]; isRedir {
				continue
			}
			redirects[u] = canonical
		}
	}

	// Remove files from a previous build that are gone now (inside the root).
	prev, _ := loadManifest(filepath.Join(o.Out, "manifest.json"))
	cur := map[string]bool{}
	for _, e := range man.Entries {
		cur[e.File] = true
	}
	for _, e := range prev.Entries {
		if cur[e.File] || e.File == "" {
			continue
		}
		p := joinRoot(o.Root, "/"+e.File)
		if safePath("/" + e.File) {
			os.Remove(p)
		}
	}

	if err := writeFragments(o, &man, mapped, redirects, res); err != nil {
		return nil, err
	}
	return res, nil
}

// canonicalURL picks the URL to keep from a group of identical bodies: prefer
// the form without a trailing slash, then the shortest.
func canonicalURL(urls []string) string {
	best := urls[0]
	for _, u := range urls[1:] {
		if score(u) < score(best) {
			best = u
		}
	}
	return best
}

func score(u string) int {
	n := len(u)
	if strings.HasSuffix(u, "/") {
		n += 1000
	}
	return n
}

func writeFragments(o MaterialiseOptions, man *Manifest, mapped, redirects map[string]string, res *MaterialiseResult) error {
	// extra.map — keyed on $request_uri, never $uri (SPIDER.md §6.2).
	var b strings.Builder
	b.WriteString("# generated by anneal - do not edit\n")
	b.WriteString("map $request_uri $resolved {\n")
	b.WriteString("    default \"\";\n")
	keys := sortedKeys(mapped)
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s  %s;\n", quoteConf(k), quoteConf(mapped[k]))
	}
	b.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(o.Out, "extra.map"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	res.Mapped = keys

	// redirects.conf
	var r strings.Builder
	r.WriteString("# generated by anneal - do not edit\n")
	rkeys := sortedKeys(redirects)
	for _, k := range rkeys {
		loc := redirects[k]
		if !strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
			loc = "/" + loc
		}
		fmt.Fprintf(&r, "location = %s { return 301 %s; }\n", quoteConf(k), quoteConf(loc))
	}
	if err := os.WriteFile(filepath.Join(o.Out, "redirects.conf"), []byte(r.String()), 0o644); err != nil {
		return err
	}
	res.Redirects = rkeys

	// manifest.json
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	mb = append(mb, '\n')
	if err := os.WriteFile(filepath.Join(o.Out, "manifest.json"), mb, 0o644); err != nil {
		return err
	}

	return writeReport(o, man, res)
}

func writeReport(o MaterialiseOptions, man *Manifest, res *MaterialiseResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# anneal report\n\n")
	fmt.Fprintf(&b, "Origin: `%s`\n\n", o.Origin)
	fmt.Fprintf(&b, "WARC: `%s`\n\n", o.WarcPath)

	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "- stored: **%d**\n", res.Stored)
	fmt.Fprintf(&b, "- skipped (non-2xx / off-origin): **%d**\n", res.Skipped)
	fmt.Fprintf(&b, "- map entries (`extra.map`): **%d**\n", len(res.Mapped))
	fmt.Fprintf(&b, "- redirects (`redirects.conf`): **%d**\n", len(res.Redirects))
	fmt.Fprintf(&b, "- hashed into `_hash/`: **%d**\n", len(res.Hashed))
	fmt.Fprintf(&b, "- refused (unsafe path): **%d**\n", len(res.Refused))
	fmt.Fprintf(&b, "\n")

	if len(res.Unmapped) > 0 {
		b.WriteString("## Unmapped Content-Types (exit 2)\n\n")
		b.WriteString("These types have no extension in `nginx/mime.types`; they would be\n")
		b.WriteString("served as `application/octet-stream` in production. Add them to\n")
		b.WriteString("`nginx/mime.types` and re-run.\n\n")
		for _, ct := range sortedKeys(res.Unmapped) {
			fmt.Fprintf(&b, "- `%s` (%d)\n", ct, res.Unmapped[ct])
		}
		b.WriteString("\n")
	}

	if len(res.Mapped) > 0 {
		b.WriteString("## Map entries\n\n")
		for _, k := range res.Mapped {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
		b.WriteString("\n")
	}
	if len(res.Redirects) > 0 {
		b.WriteString("## Redirects\n\n")
		for _, k := range res.Redirects {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
		b.WriteString("\n")
	}
	if len(res.Hashed) > 0 {
		b.WriteString("## Hashed (URL path >= 250 bytes)\n\n")
		for _, k := range res.Hashed {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
		b.WriteString("\n")
	}
	if len(res.Refused) > 0 {
		b.WriteString("## Refused paths\n\n")
		for _, k := range res.Refused {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
		b.WriteString("\n")
	}
	if len(res.OffOrigin) > 0 {
		b.WriteString("## Off-origin (not materialised)\n\n")
		for _, k := range res.OffOrigin {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Stored URLs\n\n")
	for _, e := range man.Entries {
		fmt.Fprintf(&b, "- `%s` -> `%s` (%s, %d bytes)\n", e.URL, e.File, e.CT, e.Bytes)
	}
	return os.WriteFile(filepath.Join(o.Out, "report.md"), []byte(b.String()), 0o644)
}

func firstHeader(r *WARCResponse, name string) string {
	if vs := r.Headers[strings.ToLower(name)]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// quoteConf quotes a string for nginx config, escaping quote and backslash.
func quoteConf(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- manifest I/O ----

func loadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// runMaterialise implements the `materialise` subcommand (SPIDER.md §6).
func runMaterialise(args []string) int {
	fs := flag.NewFlagSet("materialise", flag.ContinueOnError)
	var (
		warc     string
		origin   string
		root     string
		out      string
		mimePath string
	)
	fs.StringVar(&warc, "warc", "", "WARC file to read")
	fs.StringVar(&origin, "origin", "", "origin URL (https://example.com)")
	fs.StringVar(&root, "root", "", "output root for the site tree")
	fs.StringVar(&out, "out", "", "directory for generated fragments (default <root>/nginx)")
	fs.StringVar(&mimePath, "mime", "", "path to nginx/mime.types")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if warc == "" || origin == "" || root == "" {
		fmt.Fprintln(os.Stderr, "materialise: --warc, --origin and --root are required")
		return 2
	}
	if out == "" {
		out = filepath.Join(root, "nginx")
	}
	mime, err := loadMime(mimePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "materialise: %v\n", err)
		return 1
	}
	res, err := materialise(MaterialiseOptions{
		WarcPath: warc,
		Origin:   origin,
		Root:     root,
		Out:      out,
		Mime:     mime,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "materialise: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "materialise: %d stored, %d skipped, %d map entries, %d redirects\n",
		res.Stored, res.Skipped, len(res.Mapped), len(res.Redirects))
	if len(res.Unmapped) > 0 {
		fmt.Fprintf(os.Stderr, "materialise: %d Content-Type(s) had no extension\n", len(res.Unmapped))
		return 2
	}
	return 0
}

