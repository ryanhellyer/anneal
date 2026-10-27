package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// WARCRecord is one logical WARC record: its headers and raw payload.
type WARCRecord struct {
	Header  map[string]string
	Payload []byte
}

// WARCResponse is an HTTP response taken from a WARC response record.
type WARCResponse struct {
	Target      string
	Status      int
	ContentType string
	Headers     map[string][]string
	Body        []byte
}

// readWARC opens a (possibly gzipped, possibly multi-member) WARC and calls fn
// for every record. A nil record is never passed.
func readWARC(path string, fn func(*WARCRecord) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<16)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	rd := bufio.NewReaderSize(r, 1<<16)
	for {
		rec, err := readRecord(rd)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if rec == nil {
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// readRecord reads one WARC record. It returns io.EOF at a clean end of stream.
func readRecord(r *bufio.Reader) (*WARCRecord, error) {
	line, err := readLine(r)
	for line == "" && err == nil {
		line, err = readLine(r) // tolerate blank lines between records
	}
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "WARC/") {
		return nil, fmt.Errorf("warc: bad record start %q", line)
	}

	header := map[string]string{}
	contentLength := int64(-1)
	for {
		line, err = readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		header[strings.ToLower(k)] = v
		if strings.EqualFold(k, "Content-Length") {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				contentLength = n
			}
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("warc: record without Content-Length")
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("warc: reading %d-byte payload: %w", contentLength, err)
	}
	if _, err := r.Discard(4); err != nil { // "\r\n\r\n"
		return nil, err
	}
	return &WARCRecord{Header: header, Payload: payload}, nil
}

// readLine returns a line without its trailing CRLF. At EOF with a partial
// final line, it returns that line and a nil error so the caller can decide.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// parseResponse turns a response record into a WARCResponse. Non-response
// records return (nil, nil).
func parseResponse(rec *WARCRecord) (*WARCResponse, error) {
	if !strings.EqualFold(rec.Header["warc-type"], "response") {
		return nil, nil
	}
	target := rec.Header["warc-target-uri"]
	// The WARC grammar wraps the target in angle brackets: <http://host/path>.
	if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
		target = target[1 : len(target)-1]
	}
	payload := rec.Payload

	idx := bytes.Index(payload, []byte("\r\n\r\n"))
	sep := 4
	if idx < 0 {
		idx = bytes.Index(payload, []byte("\n\n"))
		sep = 2
	}
	if idx < 0 {
		return nil, fmt.Errorf("warc: no HTTP header terminator for %s", target)
	}
	head := payload[:idx]
	body := payload[idx+sep:]

	lines := strings.Split(string(head), "\n")
	status := parseStatus(strings.TrimRight(lines[0], "\r"))
	headers := map[string][]string{}
	for _, ln := range lines[1:] {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		headers[k] = append(headers[k], strings.TrimSpace(v))
	}
	ct := ""
	if vs := headers["content-type"]; len(vs) > 0 {
		ct = vs[0]
	}
	return &WARCResponse{
		Target:      target,
		Status:      status,
		ContentType: ct,
		Headers:     headers,
		Body:        body,
	}, nil
}

// parseStatus finds the 3-digit status token in a status line. It works for
// both "HTTP/1.1 200 OK" and the shorter "200 OK" shape.
func parseStatus(line string) int {
	for _, f := range strings.Fields(line) {
		if len(f) == 3 {
			if n, err := strconv.Atoi(f); err == nil && n >= 100 && n < 600 {
				return n
			}
		}
	}
	return 0
}
