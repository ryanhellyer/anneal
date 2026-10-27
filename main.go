// Anneal builds a static copy of a live website.
//
// The design is described in SPIDER.md: a crawler (wget) produces a WARC, and
// this program materialises the WARC onto disk in the URL -> file layout from
// PLAN.md, then can verify the generated tree against the crawl.
//
// Typical use:
//
//	anneal --url=https://example.com --path=./
//
// which crawls the site and writes the tree into the current directory, with
// the generated nginx fragments under <path>/nginx/.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const version = "1.0.0"

func usage() {
	fmt.Fprint(os.Stderr, `anneal - crawl a live site and write it to disk in the PLAN.md layout.

Usage:
  anneal --url=https://example.com --path=./
      Crawl --url with wget into a WARC, then materialise it under --path.
      The tree is written into --path; nginx fragments (extra.map,
      redirects.conf, manifest.json, report.md, site.warc.gz) go to
      <path>/nginx/.

  anneal crawl       --origin https://example.com [--warc site] [--seed /sitemap.xml ...]
  anneal materialise --warc site.warc.gz --origin https://example.com \
                     --root ./site --out ./site/nginx [--mime nginx/mime.types]
  anneal verify      --base http://127.0.0.1:8080 --manifest ./site/nginx/manifest.json

Flags for the full pipeline:
  --url    seed URL (required)
  --path   directory to write the site into (default "./")
  --seed   extra seed URL, repeatable (sitemap.xml, access-log URLs, ...)
  --warc   write the crawl WARC to this path (default <path>/nginx/site.warc.gz)
  --no-warc        discard the WARC instead of keeping it
  --recrawl        crawl again even if a WARC already exists (default: reuse it)
  --mime   path to nginx/mime.types (default: search, or embedded copy)
  --ignore-robots  do not honour robots.txt (only on a server you own)
  --wipe           delete --path before writing (checked: must be absolute-ish)
  --scratch        crawl scratch dir (default: <path>/.anneal-crawl, removed after)
`)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "crawl":
			return runCrawl(args[1:])
		case "materialise", "materialize":
			return runMaterialise(args[1:])
		case "verify":
			return runVerify(args[1:])
		case "help", "-h", "--help":
			usage()
			return 0
		case "version", "-v", "--version":
			fmt.Println("anneal", version)
			return 0
		}
	}
	return runPipeline(args)
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runPipeline(args []string) int {
	fs := flag.NewFlagSet("anneal", flag.ContinueOnError)
	fs.Usage = usage
	var (
		rawURL       string
		path         string
		warcDest     string
		mimePath     string
		scratch      string
		noWarc       bool
		ignoreRobots bool
		wipe         bool
		recrawl      bool
		seeds        stringList
	)
	fs.StringVar(&rawURL, "url", "", "seed URL to crawl (required)")
	fs.StringVar(&path, "path", "./", "directory to write the site into")
	fs.StringVar(&warcDest, "warc", "", "write the crawl WARC to this path")
	fs.StringVar(&mimePath, "mime", "", "path to nginx/mime.types")
	fs.StringVar(&scratch, "scratch", "", "crawl scratch dir (default: <path>/.anneal-crawl)")
	fs.BoolVar(&noWarc, "no-warc", false, "discard the WARC instead of keeping it")
	fs.BoolVar(&ignoreRobots, "ignore-robots", false, "do not honour robots.txt")
	fs.BoolVar(&wipe, "wipe", false, "delete --path before writing")
	fs.BoolVar(&recrawl, "recrawl", false, "crawl again even if a WARC already exists")
	fs.Var(&seeds, "seed", "extra seed URL (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rawURL == "" {
		fmt.Fprintln(os.Stderr, "anneal: --url is required")
		usage()
		return 2
	}

	origin, normURL, err := normaliseOrigin(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 2
	}

	root, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anneal: --path: %v\n", err)
		return 2
	}
	out := filepath.Join(root, "nginx")

	if wipe {
		if err := safeWipe(root); err != nil {
			fmt.Fprintf(os.Stderr, "anneal: refusing to wipe: %v\n", err)
			return 2
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	// wget needs scratch space for the pages it downloads before deleting them
	// (--delete-after). By default that lives inside --path, not /tmp, and is
	// removed when we are done. Override with --scratch to keep it elsewhere.
	dir := scratch
	ownScratch := false
	if dir == "" {
		dir = filepath.Join(root, ".anneal-crawl")
		ownScratch = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}
	if ownScratch {
		defer os.RemoveAll(dir)
	}

	// The WARC is written straight to where it will be kept - no copy step.
	// Default: <out>/site.warc.gz. --warc points elsewhere; --no-warc uses
	// scratch and discards it.
	var warcBase, warcDir string
	switch {
	case noWarc:
		warcBase, warcDir = filepath.Join(dir, "site"), dir
	case warcDest != "":
		warcBase = warcBaseFor(warcDest)
		warcDir = filepath.Dir(warcBase)
	default:
		warcBase, warcDir = filepath.Join(out, "site"), out
	}
	if err := os.MkdirAll(warcDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	// Reuse the WARC from a previous run instead of re-crawling. This is what
	// lets you "run it again" after an interruption without starting the fetch
	// over; --recrawl forces a fresh crawl. A WARC is only reused for the same
	// origin, so changing --url with the same --path does not serve stale bytes.
	var warcPath string
	existing := findWARC(warcBase)
	reuse := false
	if existing != "" && !recrawl {
		if prev := warcOrigin(warcBase); prev == "" || prev == origin {
			reuse = true
			fmt.Fprintf(os.Stderr, "anneal: reusing existing WARC %s (use --recrawl to fetch again)\n", existing)
		} else {
			fmt.Fprintf(os.Stderr, "anneal: existing WARC is for %s, not %s; crawling fresh\n", prev, origin)
		}
	}
	if reuse {
		warcPath = existing
	} else {
		// Crawl to a temporary WARC and move it into place only on success, so
		// a failed recrawl never destroys the previous WARC.
		tmpBase := warcBase + ".partial"
		removeWARC(tmpBase)
		defer removeWARC(tmpBase)
		fmt.Fprintf(os.Stderr, "anneal: crawling %s\n", normURL)
		tmp, err := crawlSite(CrawlOptions{
			Origin:       normURL,
			WarcBase:     tmpBase,
			Scratch:      dir,
			Seeds:        seeds,
			IgnoreRobots: ignoreRobots,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "anneal: crawl failed: %v\n", err)
			if existing != "" {
				fmt.Fprintf(os.Stderr, "anneal: kept the previous WARC at %s\n", existing)
			}
			return 1
		}
		warcPath, err = installWARC(tmp, warcBase)
		if err != nil {
			fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
			return 1
		}
	}
	writeWarcOrigin(warcBase, origin)

	mime, err := loadMime(mimePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "anneal: materialising into %s\n", root)
	res, err := materialise(MaterialiseOptions{
		WarcPath: warcPath,
		Origin:   origin,
		Root:     root,
		Out:      out,
		Mime:     mime,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr,
		"anneal: done - %d stored, %d skipped, %d map entries, %d redirects\n",
		res.Stored, res.Skipped, len(res.Mapped), len(res.Redirects))
	if !noWarc {
		fmt.Fprintf(os.Stderr, "anneal: warc: %s\n", warcPath)
	}
	fmt.Fprintf(os.Stderr, "anneal: report: %s\n", filepath.Join(out, "report.md"))

	if len(res.Unmapped) > 0 {
		fmt.Fprintf(os.Stderr, "anneal: %d Content-Type(s) had no extension (see report.md)\n", len(res.Unmapped))
		return 2
	}
	return 0
}

// normaliseOrigin returns (scheme://host, full seed URL). It tolerates a bare
// host by assuming https.
func normaliseOrigin(raw string) (origin, full string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	i := strings.Index(raw, "://")
	scheme := strings.ToLower(raw[:i])
	if scheme != "http" && scheme != "https" {
		return "", "", fmt.Errorf("unsupported scheme %q in %q", scheme, raw)
	}
	rest := raw[i+3:]
	host := rest
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		host = rest[:j]
	}
	if host == "" {
		return "", "", fmt.Errorf("cannot parse URL %q", raw)
	}
	return scheme + "://" + host, raw, nil
}

// safeWipe removes dir only when it is a plausible output root - never the
// filesystem root, never the home directory, never something outside CWD.
func safeWipe(dir string) error {
	dir = filepath.Clean(dir)
	if dir == "/" || dir == "." || dir == "" {
		return fmt.Errorf("will not remove %q", dir)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == dir {
		return fmt.Errorf("will not remove the home directory")
	}
	wd, err := os.Getwd()
	if err == nil {
		rel, err := filepath.Rel(wd, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%q is outside the working directory", dir)
		}
	}
	return os.RemoveAll(dir)
}

// warcBaseFor turns a requested WARC filename into the base wget expects: wget
// appends ".warc.gz" (or ".warc"). "--warc site.warc.gz" -> base "site".
func warcBaseFor(dest string) string {
	dest = strings.TrimSuffix(dest, ".gz")
	dest = strings.TrimSuffix(dest, ".warc")
	return dest
}

// warcOrigin reads the origin a WARC was crawled from (sidecar next to it).
func warcOrigin(base string) string {
	b, err := os.ReadFile(base + ".origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeWarcOrigin(base, origin string) {
	_ = os.WriteFile(base+".origin", []byte(origin+"\n"), 0o644)
}

// installWARC moves a freshly crawled WARC (and its CDX) into its final place,
// so the move is atomic within the directory and a failed recrawl keeps the
// previous WARC intact.
func installWARC(tmp, warcBase string) (string, error) {
	ext := ".warc.gz"
	if !strings.HasSuffix(tmp, ".warc.gz") {
		ext = ".warc"
	}
	dest := warcBase + ext
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("installing WARC: %w", err)
	}
	tmpCdx := strings.TrimSuffix(tmp, ext) + ".cdx"
	if _, err := os.Stat(tmpCdx); err == nil {
		_ = os.Rename(tmpCdx, warcBase+".cdx")
	}
	return dest, nil
}
