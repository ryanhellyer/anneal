package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CrawlOptions configures the wget wrapper (SPIDER.md §4).
type CrawlOptions struct {
	Origin       string
	WarcBase     string // wget writes <WarcBase>.warc.gz (or .warc)
	Scratch      string
	Seeds        []string
	IgnoreRobots bool
}

// crawlSite runs wget and returns the path of the WARC it produced.
//
// The optional flags differ between wget builds and versions (older or
// reduced builds reject --no-warc-keep-log and --warc-cdx), so we ask wget
// what it supports rather than assuming. Only --warc-file is required.
func crawlSite(o CrawlOptions) (string, error) {
	bin := "wget"
	if v := os.Getenv("ANNEAL_WGET"); v != "" {
		bin = v
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", fmt.Errorf("%s not found in PATH: %w", bin, err)
	}
	if err := os.MkdirAll(o.Scratch, 0o755); err != nil {
		return "", err
	}

	help := wgetHelp(bin)
	if !strings.Contains(help, "warc-file") {
		return "", fmt.Errorf("this wget (%s) has no WARC support (-warc-file); "+
			"install GNU wget >= 1.20, or use browsertrix-crawler and keep the WARC seam (SPIDER.md §4)", bin)
	}

	// Try the fully-featured command, then a minimal command that uses only
	// options every wget build supports.
	err := runWget(bin, buildWgetArgs(o, help, true))
	warc := findWARC(o.WarcBase)
	if warc == "" && isUsageError(err) {
		removeWARC(o.WarcBase)
		err = runWget(bin, buildWgetArgs(o, help, false))
		warc = findWARC(o.WarcBase)
	}
	if warc == "" {
		if err != nil {
			return "", fmt.Errorf("%s failed: %w", bin, err)
		}
		return "", fmt.Errorf("%s produced no WARC", bin)
	}
	// wget exit 8 means "server issued an error" (404s etc.) during a crawl
	// that otherwise completed; that is normal and not fatal.
	if err != nil && !isServerError(err) {
		return "", fmt.Errorf("%s failed: %w", bin, err)
	}
	return warc, nil
}

// buildWgetArgs assembles the wget command. With full=false only the options
// that every wget supports (plus --warc-file) are used.
func buildWgetArgs(o CrawlOptions, help string, full bool) []string {
	// --timeout/--tries/--waitretry bound the crawl so a dead connection cannot
	// hang it forever; they are old, universally supported options.
	args := []string{
		"--recursive", "--level=inf",
		"--timeout=30", "--tries=3", "--waitretry=5",
	}
	if strings.Contains(help, "retry-connrefused") {
		args = append(args, "--retry-connrefused")
	}
	if full {
		args = append(args, "--page-requisites", "--no-parent")
		if strings.Contains(help, "delete-after") {
			args = append(args, "--delete-after")
		}
		if strings.Contains(help, "no-verbose") || strings.Contains(help, "-nv,") {
			args = append(args, "--no-verbose")
		}
	}
	args = append(args, "--warc-file="+o.WarcBase)
	if full {
		if strings.Contains(help, "warc-cdx") {
			args = append(args, "--warc-cdx")
		}
		if strings.Contains(help, "warc-keep-log") {
			args = append(args, "--no-warc-keep-log")
		}
	}
	args = append(args, "-P", o.Scratch)
	if o.IgnoreRobots {
		args = append(args, "-e", "robots=off")
	}
	args = append(args, o.Origin)
	args = append(args, o.Seeds...)
	return args
}

func runWget(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func isUsageError(err error) bool {
	ee, ok := err.(*exec.ExitError)
	return ok && ee.ExitCode() == 2
}

func isServerError(err error) bool {
	ee, ok := err.(*exec.ExitError)
	return ok && ee.ExitCode() == 8
}

func removeWARC(base string) {
	os.Remove(base + ".warc.gz")
	os.Remove(base + ".warc")
	os.Remove(base + ".cdx")
	os.Remove(base + ".origin")
}

// findWARC returns the WARC wget wrote: compressed by default, but some builds
// write an uncompressed .warc.
func findWARC(base string) string {
	for _, p := range []string{base + ".warc.gz", base + ".warc"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func wgetHelp(bin string) string {
	out, _ := exec.Command(bin, "--help").CombinedOutput()
	return string(out)
}

func runCrawl(args []string) int {
	fs := flag.NewFlagSet("crawl", flag.ContinueOnError)
	var (
		origin       string
		warc         string
		scratch      string
		ignoreRobots bool
		seeds        stringList
	)
	fs.StringVar(&origin, "origin", "", "origin URL to crawl (https://example.com)")
	fs.StringVar(&warc, "warc", "site", "WARC base path (without .warc.gz)")
	fs.StringVar(&scratch, "scratch", "", "scratch directory (default: next to the WARC)")
	fs.BoolVar(&ignoreRobots, "ignore-robots", false, "do not honour robots.txt")
	fs.Var(&seeds, "seed", "extra seed URL (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if origin == "" {
		fmt.Fprintln(os.Stderr, "crawl: --origin is required")
		return 2
	}
	dir := scratch
	if dir == "" {
		dir = filepath.Dir(warc)
	}
	removeWARC(warc)
	path, err := crawlSite(CrawlOptions{
		Origin:       origin,
		WarcBase:     warc,
		Scratch:      dir,
		Seeds:        seeds,
		IgnoreRobots: ignoreRobots,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "crawl: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "crawl: wrote %s\n", path)
	return 0
}
