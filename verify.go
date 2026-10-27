package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// runVerify re-serves the generated tree and compares every manifest entry
// against what the crawler captured (SPIDER.md §9). Exit 1 on any mismatch.
func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var (
		base     string
		manifest string
		timeout  time.Duration
	)
	fs.StringVar(&base, "base", "", "base URL of the running server (http://127.0.0.1:8080)")
	fs.StringVar(&manifest, "manifest", "", "path to manifest.json")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if base == "" || manifest == "" {
		fmt.Fprintln(os.Stderr, "verify: --base and --manifest are required")
		return 2
	}
	base = strings.TrimRight(base, "/")

	man, err := loadManifest(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		return 1
	}

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // observe 3xx, do not follow it
		},
	}

	var failures []string
	for _, e := range man.Entries {
		status, body, ct, err := fetch(client, base, e.URL)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: request failed: %v", e.URL, err))
			continue
		}
		if status != e.Status {
			failures = append(failures, fmt.Sprintf("%s: status %d, want %d", e.URL, status, e.Status))
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != e.SHA256 {
			failures = append(failures, fmt.Sprintf("%s: body sha256 %s, want %s", e.URL, got, e.SHA256))
		}
		if normalizeMediaType(ct) != normalizeMediaType(e.CT) {
			failures = append(failures, fmt.Sprintf("%s: Content-Type %q, want %q", e.URL, ct, e.CT))
		}
		if strings.HasSuffix(e.URL, "/") && status == 403 {
			failures = append(failures, fmt.Sprintf("%s: directory URL returns 403", e.URL))
		}
	}

	sort.Strings(failures)
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "verify: %d failure(s) out of %d URLs\n", len(failures), len(man.Entries))
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  "+f)
		}
		return 1
	}
	fmt.Printf("verify: OK - %d URLs match\n", len(man.Entries))
	return 0
}

// fetch requests base+raw with the raw path preserved (no re-encoding) so the
// server sees the same bytes the crawler did.
func fetch(client *http.Client, base, raw string) (int, []byte, string, error) {
	req, err := http.NewRequest("GET", base, nil)
	if err != nil {
		return 0, nil, "", err
	}
	// Keep the raw request URI: $request_uri is what the map is keyed on, and
	// %2F etc. must not be re-encoded (SPIDER.md §9).
	req.URL = &url.URL{Scheme: req.URL.Scheme, Host: req.URL.Host, Opaque: raw}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, "", err
	}
	return resp.StatusCode, body, resp.Header.Get("Content-Type"), nil
}
