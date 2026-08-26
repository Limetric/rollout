package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Resumable uploads for the media the Publisher API takes: app bundles and
// APKs, store-listing images, and deobfuscation/native-symbol files.
//
// Google's resumable protocol is two phases. An initiate request declares the
// content type and length and gets back a session URL; the bytes then go up in
// chunks, each answered with either 308 (send more, here is what I have) or a
// final 2xx carrying the resource. Simple uploads exist but cap out well below
// the size of a real app bundle, and give up the whole transfer on one dropped
// connection.

// uploadChunkSize is the per-request payload. Google requires every chunk
// except the last to be a multiple of 256 KiB; 8 MiB is their recommended
// starting point and keeps the number of round trips sane for a 200 MB bundle.
const uploadChunkSize = 8 << 20

// uploadChunkTimeout bounds a single chunk request. Google's guidance is to cap
// each request rather than the whole transfer, so a stalled connection is
// retried at the chunk boundary instead of failing a half-hour upload.
const uploadChunkTimeout = 2 * time.Minute

// uploadMaxRetriesPerChunk bounds how many times one chunk is re-sent. The
// resumable protocol makes a chunk safe to repeat — the server reports the
// range it holds and duplicate bytes are discarded — which is why uploads can
// retry where a commit cannot.
const uploadMaxRetriesPerChunk = 5

// uploadProgress is called after each chunk lands, for the CLI's progress line.
// It is nil under MCP and with --quiet.
type uploadProgress func(sent, total int64)

// uploadMedia sends a file to an upload endpoint using the resumable protocol
// and decodes the final resource into out.
//
// path is relative to the Publisher API root (e.g.
// "applications/com.example.app/edits/123/bundles"); the upload host and the
// /upload prefix are added here.
func (c *Client) uploadMedia(ctx context.Context, path, contentType, filePath string, query url.Values, out any, progress uploadProgress) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open %q: %w", filePath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", filePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, not a file", filePath)
	}
	size := info.Size()
	if size == 0 {
		return fmt.Errorf("%q is empty", filePath)
	}

	sessionURL, err := c.initiateUpload(ctx, path, contentType, size, query)
	if err != nil {
		return err
	}
	return c.sendChunks(ctx, sessionURL, f, size, out, progress)
}

// initiateUpload opens a resumable session and returns its URL.
func (c *Client) initiateUpload(ctx context.Context, path, contentType string, size int64, query url.Values) (string, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("uploadType", "resumable")
	rawURL := c.cfg.BaseURL + "/upload/" + publisherAPIPath + "/" + path + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", playUserAgent())
	req.Header.Set("X-Upload-Content-Type", contentType)
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	req.Header.Set("Content-Length", "0")

	resp, err := c.upload.Do(req)
	if err != nil {
		return "", fmt.Errorf("start upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return "", fmt.Errorf("start upload: %w", parseAPIError(resp.StatusCode, body))
	}
	session := resp.Header.Get("Location")
	if session == "" {
		// Without a session URL there is nowhere to send the bytes, and
		// continuing would produce a confusing failure one request later.
		return "", fmt.Errorf("start upload: the API returned no upload session URL")
	}
	return session, nil
}

// sendChunks streams the file to the session URL, resuming from whatever range
// the server reports on a 308.
func (c *Client) sendChunks(ctx context.Context, sessionURL string, f *os.File, size int64, out any, progress uploadProgress) error {
	var offset int64
	retries := 0
	stalled := 0
	for offset < size {
		before := offset
		end := min(offset+uploadChunkSize, size)
		status, body, serverEnd, err := c.sendChunk(ctx, sessionURL, f, offset, end, size)
		if err != nil {
			// A transport failure mid-transfer is the case resumable uploads
			// exist for: ask the server what it holds and carry on from there.
			if retries >= uploadMaxRetriesPerChunk {
				return fmt.Errorf("upload chunk at byte %d: %w", offset, err)
			}
			retries++
			resumed, queryErr := c.queryUploadOffset(ctx, sessionURL, size)
			if queryErr != nil {
				return fmt.Errorf("upload chunk at byte %d: %w", offset, err)
			}
			offset = resumed
			continue
		}
		retries = 0

		switch {
		case status == 308:
			// The server states the last byte it holds, which may be less than
			// what we sent. Trusting our own count instead would silently skip
			// bytes and produce a corrupt artifact.
			offset = serverEnd
		case status < 300:
			if out != nil && len(body) > 0 {
				if err := json.Unmarshal(body, out); err != nil {
					return fmt.Errorf("decode upload response: %w", err)
				}
			}
			if progress != nil {
				progress(size, size)
			}
			return nil
		default:
			return parseAPIError(status, body)
		}
		if progress != nil {
			progress(offset, size)
		}
		// A server that keeps acknowledging the same range is not going to
		// finish, and spinning on it would hang the CLI with no output at all.
		if offset <= before {
			stalled++
			if stalled >= uploadMaxRetriesPerChunk {
				return fmt.Errorf("upload stalled at byte %d of %d — the API kept acknowledging the same range", offset, size)
			}
		} else {
			stalled = 0
		}
	}
	return fmt.Errorf("upload finished sending %d bytes but the API never returned a result", size)
}

// sendChunk PUTs one range and reports the status, the body, and — for a 308 —
// the first byte the server still needs.
func (c *Client) sendChunk(ctx context.Context, sessionURL string, f *os.File, start, end, size int64) (status int, body []byte, nextOffset int64, err error) {
	chunkCtx, cancel := context.WithTimeout(ctx, uploadChunkTimeout)
	defer cancel()

	section := io.NewSectionReader(f, start, end-start)
	req, err := http.NewRequestWithContext(chunkCtx, http.MethodPut, sessionURL, section)
	if err != nil {
		return 0, nil, 0, err
	}
	req.ContentLength = end - start
	req.Header.Set("User-Agent", playUserAgent())
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))

	resp, err := c.upload.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, 0, err
	}
	if resp.StatusCode == 308 {
		next, err := parseUploadRange(resp.Header.Get("Range"))
		if err != nil {
			return 0, nil, 0, err
		}
		return resp.StatusCode, body, next, nil
	}
	return resp.StatusCode, body, 0, nil
}

// queryUploadOffset asks how much of the file the server already holds, which
// is how an interrupted upload resumes instead of starting over.
func (c *Client) queryUploadOffset(ctx context.Context, sessionURL string, size int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
	if err != nil {
		return 0, err
	}
	req.ContentLength = 0
	req.Header.Set("User-Agent", playUserAgent())
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))

	resp, err := c.upload.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 308 {
		return 0, fmt.Errorf("resume upload: unexpected status %d", resp.StatusCode)
	}
	return parseUploadRange(resp.Header.Get("Range"))
}

// parseUploadRange reads Google's "bytes=0-8388607" resume header and returns
// the first byte still needed. An absent header means the server holds nothing
// yet, which is a resume from zero rather than an error.
func parseUploadRange(header string) (int64, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, nil
	}
	_, rangeSpec, ok := strings.Cut(header, "=")
	if !ok {
		rangeSpec = header
	}
	_, last, ok := strings.Cut(rangeSpec, "-")
	if !ok {
		return 0, fmt.Errorf("unparseable upload Range header %q", header)
	}
	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable upload Range header %q: %w", header, err)
	}
	return end + 1, nil
}

// contentTypeForArtifact maps an artifact path to the content type the
// Publisher API expects. Getting this wrong is rejected at initiate time with a
// message that does not mention the file, so it is decided here from the
// extension the user actually passed.
func contentTypeForArtifact(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".aab", ".apk":
		return "application/octet-stream", nil
	case ".png":
		return "image/png", nil
	case ".jpg", ".jpeg":
		return "image/jpeg", nil
	case ".zip", ".txt", ".sym":
		return "application/octet-stream", nil
	default:
		return "", fmt.Errorf("cannot tell what %q is — expected .aab, .apk, .png, .jpg, .zip, or .txt", path)
	}
}
