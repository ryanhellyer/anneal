package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// CrawlOptions configures the wget wrapper (SPIDER.md §4).
type CrawlOptions struct {
	Origin       string
	WarcBase     string // wget writes <WarcBase>.warc.gz
	Scratch      string
	Seeds        []string
	IgnoreRobots bool
}

// crawlSite runs wget and leaves a WARC at WarcBase+".warc.gz".
func crawlSite(o CrawlOptions) error {
	if _, err := exec.LookPath("wget"); err != nil {
		return fmt.Errorf("wget not found in PATH: %w", err)
	}
	if err := os.MkdirAll(o.Scratch, 0o755); err != nil {
		return err
	}

	args := []string{
		"--recursive",
		"--level=inf",
		"--page-requisites",
		"--no-parent",
		"--delete-after",
		"--no-warc-keep-log",
		"--no-verbose",
		"--warc-file=" + o.WarcBase,
		"--warc-cdx",
		"-P", o.Scratch,
	}
	if o.IgnoreRobots {
		args = append(args, "-e", "robots=off")
	}
	args = append(args, o.Origin)
	args = append(args, o.Seeds...)

	cmd := exec.Command("wget", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	warc := o.WarcBase + ".warc.gz"
	if _, statErr := os.Stat(warc); statErr != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("wget produced no WARC")
	}
	// wget exit 8 means "server issued an error" (404s etc.) during a crawl
	// that otherwise completed; that is normal and not fatal.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 8 {
			return nil
		}
		return err
	}
	return nil
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
	if err := crawlSite(CrawlOptions{
		Origin:       origin,
		WarcBase:     warc,
		Scratch:      dir,
		Seeds:        seeds,
		IgnoreRobots: ignoreRobots,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "crawl: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "crawl: wrote %s.warc.gz\n", warc)
	return 0
}
