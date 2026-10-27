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
  --warc   copy the crawl WARC to this path
  --no-warc        discard the WARC instead of keeping it
  --mime   path to nginx/mime.types (default: search, or embedded copy)
  --ignore-robots  do not honour robots.txt (only on a server you own)
  --wipe           delete --path before writing (checked: must be absolute-ish)
  --scratch        directory for crawl scratch (default: a temp dir)
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
		seeds        stringList
	)
	fs.StringVar(&rawURL, "url", "", "seed URL to crawl (required)")
	fs.StringVar(&path, "path", "./", "directory to write the site into")
	fs.StringVar(&warcDest, "warc", "", "copy the crawl WARC to this path")
	fs.StringVar(&mimePath, "mime", "", "path to nginx/mime.types")
	fs.StringVar(&scratch, "scratch", "", "crawl scratch directory (default: temp)")
	fs.BoolVar(&noWarc, "no-warc", false, "discard the WARC instead of keeping it")
	fs.BoolVar(&ignoreRobots, "ignore-robots", false, "do not honour robots.txt")
	fs.BoolVar(&wipe, "wipe", false, "delete --path before writing")
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	dir := scratch
	if dir == "" {
		dir, err = os.MkdirTemp("", "anneal-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
			return 1
		}
		defer os.RemoveAll(dir)
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: %v\n", err)
		return 1
	}

	warcBase := filepath.Join(dir, "site")

	fmt.Fprintf(os.Stderr, "anneal: crawling %s\n", normURL)
	if err := crawlSite(CrawlOptions{
		Origin:       normURL,
		WarcBase:     warcBase,
		Scratch:      dir,
		Seeds:        seeds,
		IgnoreRobots: ignoreRobots,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: crawl failed: %v\n", err)
		return 1
	}
	warcPath := warcBase + ".warc.gz"
	if _, err := os.Stat(warcPath); err != nil {
		fmt.Fprintf(os.Stderr, "anneal: crawl produced no WARC at %s\n", warcPath)
		return 1
	}

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

	// Keep the WARC: it is the audit trail (SPIDER.md §14).
	if !noWarc {
		dest := warcDest
		if dest == "" {
			dest = filepath.Join(out, "site.warc.gz")
		}
		if dest != warcPath {
			if err := copyFile(warcPath, dest); err != nil {
				fmt.Fprintf(os.Stderr, "anneal: keeping WARC: %v\n", err)
			}
		}
	}

	fmt.Fprintf(os.Stderr,
		"anneal: done - %d stored, %d skipped, %d map entries, %d redirects\n",
		res.Stored, res.Skipped, len(res.Mapped), len(res.Redirects))
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

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
