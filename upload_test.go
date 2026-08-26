package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeTempFile creates a file of n bytes with a recognizable pattern, so a
// test can assert the server received exactly the right bytes in the right
// order.
func writeTempFile(t *testing.T, name string, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// resumableServer is a fake implementation of Google's resumable protocol. It
// accepts the initiate request, then answers each chunk with a 308 until the
// file is complete.
type resumableServer struct {
	received  []byte
	sessions  int
	chunkPuts int
	// failAfter drops the connection after this many chunk PUTs, once.
	failAfter int
	failed    bool
	// shortAck acknowledges fewer bytes than were sent, which a real server
	// does when it buffers partially.
	shortAck int
}

func (s *resumableServer) handler(t *testing.T, initiatePath string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("X-Upload-Content-Type") == "" || r.Header.Get("X-Upload-Content-Length") == "" {
				t.Errorf("initiate is missing the upload content headers: %v", r.Header)
			}
			if !strings.Contains(r.URL.Path, initiatePath) {
				t.Errorf("initiate path = %q, want it to contain %q", r.URL.Path, initiatePath)
			}
			if r.URL.Query().Get("uploadType") != "resumable" {
				t.Errorf("initiate query = %q, want uploadType=resumable", r.URL.RawQuery)
			}
			s.sessions++
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
			return
		}

		contentRange := r.Header.Get("Content-Range")
		// A "bytes */N" probe asks what the server holds.
		if strings.HasPrefix(contentRange, "bytes */") {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.received)-1))
			w.WriteHeader(308)
			return
		}

		s.chunkPuts++
		body, _ := io.ReadAll(r.Body)
		if s.failAfter > 0 && !s.failed && s.chunkPuts > s.failAfter {
			s.failed = true
			// Simulate a dropped connection mid-transfer.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close()
			return
		}

		accepted := len(body)
		if s.shortAck > 0 && accepted > s.shortAck {
			accepted = s.shortAck
		}
		s.received = append(s.received, body[:accepted]...)

		_, totalPart, ok := strings.Cut(contentRange, "/")
		if !ok {
			t.Errorf("unparseable Content-Range %q", contentRange)
		}
		total, err := strconv.Atoi(totalPart)
		if err != nil {
			t.Errorf("unparseable Content-Range %q: %v", contentRange, err)
		}
		if len(s.received) >= total {
			writeJSON(w, http.StatusOK, `{"versionCode":42,"sha256":"abc"}`)
			return
		}
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.received)-1))
		w.WriteHeader(308)
	}
}

func TestResumableUploadSendsEveryByte(t *testing.T) {
	// Two full chunks plus a partial one, so the chunking is actually exercised.
	const size = uploadChunkSize*2 + 1234
	path := writeTempFile(t, "app.aab", size)

	srv := &resumableServer{}
	api := newFakePlayAPI(t, srv.handler(t, "applications/com.example.app/edits/edit-1/bundles"))
	client := newTestClient(t, api)

	var out struct {
		VersionCode int `json:"versionCode"`
	}
	err := client.uploadMedia(context.Background(),
		"applications/com.example.app/edits/edit-1/bundles",
		"application/octet-stream", path, nil, &out, nil)
	if err != nil {
		t.Fatalf("uploadMedia: %v", err)
	}
	if out.VersionCode != 42 {
		t.Errorf("decoded %+v", out)
	}
	if len(srv.received) != size {
		t.Fatalf("server received %d bytes, want %d", len(srv.received), size)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(srv.received) != string(original) {
		t.Error("the uploaded bytes do not match the file")
	}
	if srv.chunkPuts != 3 {
		t.Errorf("sent %d chunks, want 3 for %d bytes", srv.chunkPuts, size)
	}
}

// TestResumableUploadResumesFromTheServersRange: trusting our own byte count
// after a short acknowledgement would skip bytes and produce a corrupt bundle.
func TestResumableUploadResumesFromTheServersRange(t *testing.T) {
	const size = uploadChunkSize + 1000
	path := writeTempFile(t, "app.aab", size)

	srv := &resumableServer{shortAck: uploadChunkSize / 2}
	api := newFakePlayAPI(t, srv.handler(t, "bundles"))
	client := newTestClient(t, api)

	if err := client.uploadMedia(context.Background(),
		"applications/com.example.app/edits/edit-1/bundles",
		"application/octet-stream", path, nil, nil, nil); err != nil {
		t.Fatalf("uploadMedia: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(srv.received) != string(original) {
		t.Errorf("received %d of %d bytes, and they do not match the file", len(srv.received), size)
	}
}

// TestResumableUploadSurvivesADroppedConnection is the whole reason the
// resumable protocol is used instead of a simple upload.
func TestResumableUploadSurvivesADroppedConnection(t *testing.T) {
	const size = uploadChunkSize*2 + 10
	path := writeTempFile(t, "app.aab", size)

	srv := &resumableServer{failAfter: 1}
	api := newFakePlayAPI(t, srv.handler(t, "bundles"))
	client := newTestClient(t, api)

	if err := client.uploadMedia(context.Background(),
		"applications/com.example.app/edits/edit-1/bundles",
		"application/octet-stream", path, nil, nil, nil); err != nil {
		t.Fatalf("uploadMedia: %v", err)
	}
	if !srv.failed {
		t.Fatal("the test never exercised the failure path")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(srv.received) != string(original) {
		t.Error("the resumed upload does not match the file")
	}
	// One session: a resume must not restart the whole transfer.
	if srv.sessions != 1 {
		t.Errorf("opened %d upload sessions, want 1", srv.sessions)
	}
}

func TestUploadReportsProgress(t *testing.T) {
	const size = uploadChunkSize*2 + 5
	path := writeTempFile(t, "app.aab", size)

	srv := &resumableServer{}
	api := newFakePlayAPI(t, srv.handler(t, "bundles"))
	client := newTestClient(t, api)

	var lastSent, lastTotal int64
	var updates int
	err := client.uploadMedia(context.Background(),
		"applications/com.example.app/edits/edit-1/bundles",
		"application/octet-stream", path, nil, nil,
		func(sent, total int64) { updates++; lastSent, lastTotal = sent, total })
	if err != nil {
		t.Fatalf("uploadMedia: %v", err)
	}
	if updates == 0 {
		t.Fatal("no progress was reported")
	}
	if lastSent != size || lastTotal != size {
		t.Errorf("final progress = %d/%d, want %d/%d", lastSent, lastTotal, size, size)
	}
}

func TestUploadRejectsUnusableFiles(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	client := newTestClient(t, api)

	t.Run("missing file", func(t *testing.T) {
		err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", "/nope/app.aab", nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "/nope/app.aab") {
			t.Fatalf("err = %v, want one naming the file", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		// An empty artifact would open a session the API then rejects with an
		// error that says nothing about the file.
		path := writeTempFile(t, "empty.aab", 0)
		err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", path, nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want one naming the problem", err)
		}
	})

	t.Run("a directory", func(t *testing.T) {
		err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", t.TempDir(), nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("err = %v, want one naming the problem", err)
		}
	})
}

// TestUploadFailsWithoutASessionURL: continuing without one produces a
// confusing failure a request later.
func TestUploadFailsWithoutASessionURL(t *testing.T) {
	path := writeTempFile(t, "app.aab", 100)
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // no Location header
	})
	client := newTestClient(t, api)

	err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", path, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "session URL") {
		t.Fatalf("err = %v, want one naming the missing session URL", err)
	}
}

func TestUploadSurfacesAPIRejections(t *testing.T) {
	path := writeTempFile(t, "app.aab", 100)
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
	})
	client := newTestClient(t, api)

	err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", path, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Users & permissions") {
		t.Fatalf("err = %v, want the actionable permission message", err)
	}
}

func TestParseUploadRange(t *testing.T) {
	tests := []struct {
		header  string
		want    int64
		wantErr bool
	}{
		{"bytes=0-8388607", 8388608, false},
		{"0-99", 100, false},
		{"", 0, false}, // nothing received yet: resume from zero
		{"bytes=garbage", 0, true},
	}
	for _, tc := range tests {
		got, err := parseUploadRange(tc.header)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseUploadRange(%q) should have failed", tc.header)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseUploadRange(%q) = %d, %v; want %d", tc.header, got, err, tc.want)
		}
	}
}

func TestContentTypeForArtifact(t *testing.T) {
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{"app.aab", "application/octet-stream", false},
		{"app.APK", "application/octet-stream", false},
		{"icon.png", "image/png", false},
		{"feature.jpg", "image/jpeg", false},
		{"feature.jpeg", "image/jpeg", false},
		{"mapping.txt", "application/octet-stream", false},
		{"symbols.zip", "application/octet-stream", false},
		{"README", "", true},
		{"app.ipa", "", true},
	}
	for _, tc := range tests {
		got, err := contentTypeForArtifact(tc.path)
		if tc.wantErr {
			if err == nil {
				t.Errorf("contentTypeForArtifact(%q) should have failed", tc.path)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("contentTypeForArtifact(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
}

// TestUploadChunkSizeIsAValidChunk: Google requires every chunk but the last to
// be a multiple of 256 KiB, and a violation is rejected mid-transfer.
func TestUploadChunkSizeIsAValidChunk(t *testing.T) {
	if uploadChunkSize%(256<<10) != 0 {
		t.Errorf("chunk size %d is not a multiple of 256 KiB", uploadChunkSize)
	}
}

// TestUploadFailsWhenTheServerNeverAdvances: a server that keeps acknowledging
// the same range is not going to finish, and spinning on it would hang the CLI
// with no output at all.
func TestUploadFailsWhenTheServerNeverAdvances(t *testing.T) {
	path := writeTempFile(t, "app.aab", uploadChunkSize*2)

	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Range", "bytes=0-99")
		w.WriteHeader(308)
	})
	client := newTestClient(t, api)

	err := client.uploadMedia(context.Background(), "bundles", "application/octet-stream", path, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v, want a stall report", err)
	}
}
