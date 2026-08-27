package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testReportsBucket is the export bucket every reports test reads.
const testReportsBucket = "pubsite_prod_rev_1234567890"

// fakeBucket is an in-memory reports bucket: object name → contents.
type fakeBucket map[string][]byte

// newBucketAPI serves the two Cloud Storage calls rollout makes — objects.list
// under a prefix, and objects.get?alt=media — over an in-memory bucket.
func newBucketAPI(t *testing.T, bucket fakeBucket) *fakePlayAPI {
	t.Helper()
	prefix := "/" + storageAPIPath + "/b/" + testReportsBucket + "/o"
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == prefix:
			serveBucketListing(w, r, bucket)
		case strings.HasPrefix(r.URL.Path, prefix+"/"):
			// %2F in the object name arrives decoded, so the remainder of the
			// path is the object name as stored.
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			body, ok := bucket[name]
			if !ok {
				writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"No such object","status":"NOT_FOUND"}}`)
				return
			}
			if r.URL.Query().Get("alt") != "media" {
				// Without alt=media the real API answers with the object's
				// metadata, which is how the --object path learns its size.
				meta, _ := json.Marshal(storageObject{
					Name: name, Size: fmt.Sprint(len(body)), Updated: "2026-08-01T03:00:00.000Z",
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(meta)
				return
			}
			w.Header().Set("Content-Type", "text/csv")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Not found","status":"NOT_FOUND"}}`)
		}
	})
}

// serveBucketListing answers objects.list, honouring prefix, maxResults, and
// the page token, so the paging path is exercised rather than assumed.
func serveBucketListing(w http.ResponseWriter, r *http.Request, bucket fakeBucket) {
	names := make([]string, 0, len(bucket))
	for name := range bucket {
		if strings.HasPrefix(name, r.URL.Query().Get("prefix")) {
			names = append(names, name)
		}
	}
	// Cloud Storage lists lexicographically; so does this, so page boundaries
	// are stable.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	start := 0
	if token := r.URL.Query().Get("pageToken"); token != "" {
		for i, name := range names {
			if name == token {
				start = i
				break
			}
		}
	}
	// One object per page: enough to prove the caller follows nextPageToken.
	page := struct {
		Items         []storageObject `json:"items"`
		NextPageToken string          `json:"nextPageToken,omitempty"`
	}{}
	if start < len(names) {
		name := names[start]
		page.Items = append(page.Items, storageObject{
			Name:    name,
			Size:    fmt.Sprint(len(bucket[name])),
			Updated: "2026-08-01T03:00:00.000Z",
		})
		if start+1 < len(names) {
			page.NextPageToken = names[start+1]
		}
	}
	body, _ := json.Marshal(page)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// newBucketClient builds a Client whose reports bucket is the given fake.
func newBucketClient(t *testing.T, bucket fakeBucket) (*Client, *fakePlayAPI) {
	t.Helper()
	api := newBucketAPI(t, bucket)
	client := newTestClient(t, api)
	client.cfg.ReportsBucket = testReportsBucket
	return client, api
}

func TestListStorageObjectsFollowsPaging(t *testing.T) {
	client, api := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202606_overview.csv": []byte("a"),
		"stats/installs/installs_com.example.app_202607_overview.csv": []byte("bb"),
		"stats/ratings/ratings_com.example.app_202607_overview.csv":   []byte("ccc"),
	})

	objects, truncated, err := client.listStorageObjects(context.Background(), testReportsBucket, "stats/installs/")
	if err != nil {
		t.Fatalf("listStorageObjects: %v", err)
	}
	if truncated {
		t.Error("a three-object bucket should not report truncation")
	}
	if len(objects) != 2 {
		t.Fatalf("objects = %v, want only the installs prefix", objects)
	}
	if objects[0].Name != "stats/installs/installs_com.example.app_202606_overview.csv" {
		t.Errorf("objects are not sorted by name: %v", objects)
	}
	if objects[1].sizeBytes() != 2 {
		t.Errorf("size = %d, want 2", objects[1].sizeBytes())
	}
	// The prefix has to reach the API: filtering client-side would download a
	// listing of the whole developer account to answer a one-app question.
	if got := api.seen()[0].Query; !strings.Contains(got, "prefix=stats%2Finstalls%2F") {
		t.Errorf("first listing query = %q, want a prefix filter", got)
	}
}

func TestDownloadStorageObjectEscapesTheWholeName(t *testing.T) {
	body := utf16LE("Date,Daily Average Rating\n2026-07-01,4.5\n")
	client, api := newBucketClient(t, fakeBucket{
		"stats/ratings/ratings_com.example.app_202607_overview.csv": body,
	})

	got, err := client.downloadStorageObject(context.Background(), testReportsBucket,
		"stats/ratings/ratings_com.example.app_202607_overview.csv")
	if err != nil {
		t.Fatalf("downloadStorageObject: %v", err)
	}
	if string(got) != string(body) {
		t.Error("downloaded bytes do not match the object")
	}
	// The JSON API takes the object name as one path segment: an unescaped
	// slash would address a different resource entirely.
	req := api.seen()[0]
	if !strings.Contains(req.RequestURI, "%2F") {
		t.Errorf("object name was not path-escaped: %s", req.RequestURI)
	}
	if !strings.Contains(req.Query, "alt=media") {
		t.Errorf("query = %q, want alt=media", req.Query)
	}
	// A CSV is not JSON, so the download must not claim to accept only JSON.
	if got := req.Header.Get("Accept"); got == "application/json" {
		t.Errorf("Accept = %q, want a media-friendly value", got)
	}
}

func TestDownloadStorageObjectMissing(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{})
	_, err := client.downloadStorageObject(context.Background(), testReportsBucket, "stats/installs/nope.csv")
	if err == nil {
		t.Fatal("expected an error for a missing object")
	}
	if !strings.Contains(err.Error(), "gs://"+testReportsBucket) {
		t.Errorf("error should name the object read: %v", err)
	}
}

func TestReportsBucketUnsetNamesTheFix(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{})
	client.cfg.ReportsBucket = ""
	_, err := client.reportsBucket()
	if err == nil {
		t.Fatal("expected an error with no bucket configured")
	}
	for _, want := range []string{"set-reports-bucket", "PLAY_REPORTS_BUCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestListStorageObjectsReportsAnUnreadTail(t *testing.T) {
	// The fake serves one object per page, so a bucket larger than maxPages is
	// the cheapest way to reach the cap. A listing that stopped early must say
	// so: presenting it as complete would make "no such month" a lie.
	bucket := fakeBucket{}
	for i := 0; i < maxPages+1; i++ {
		bucket[fmt.Sprintf("stats/installs/installs_com.example.app_2026%02d_overview.csv", i%12+1)+fmt.Sprint(i)] = []byte("x")
	}
	client, _ := newBucketClient(t, bucket)

	objects, truncated, err := client.listStorageObjects(context.Background(), testReportsBucket, "stats/installs/")
	if err != nil {
		t.Fatalf("listStorageObjects: %v", err)
	}
	if !truncated {
		t.Error("a listing that hit the page cap should report truncation")
	}
	if len(objects) != maxPages {
		t.Errorf("objects = %d, want the %d pages that were read", len(objects), maxPages)
	}
}

// TestDownloadStorageObjectRetries: a media download goes through the same
// request plumbing as every JSON call, so a 503 has to back off and retry
// rather than surface as a failed report.
func TestDownloadStorageObjectRetries(t *testing.T) {
	shrinkBackoff(t)
	body := utf16LE("Date,Daily Device Installs\n2026-07-01,120\n")
	var attempts int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "alt=media") {
			writeJSON(w, http.StatusOK, `{"items":[]}`)
			return
		}
		attempts++
		if attempts < 3 {
			writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"backend error","status":"UNAVAILABLE"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	client := newTestClient(t, api)
	client.cfg.ReportsBucket = testReportsBucket

	got, err := client.downloadStorageObject(context.Background(), testReportsBucket, "stats/installs/x.csv")
	if err != nil {
		t.Fatalf("downloadStorageObject: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want two retries before success", attempts)
	}
	if string(got) != string(body) {
		t.Error("the retried download returned the wrong bytes")
	}
}

// TestDownloadStorageObjectIsBounded: the streaming client has no timeout of
// its own, and a CLI command runs on a background context — without a deadline
// here a stalled response would hang `rollout play reports get` forever.
func TestDownloadStorageObjectIsBounded(t *testing.T) {
	if reportDownloadTimeout <= 60*time.Second {
		t.Errorf("report download timeout = %v, want more than the 60s API cap", reportDownloadTimeout)
	}

	stalled := make(chan struct{})
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		<-stalled
	})
	t.Cleanup(func() { close(stalled) })
	client := newTestClient(t, api)

	// Cancelling the caller's context has to stop the download; the deadline
	// above is the same mechanism with a clock attached.
	ctx, cancel := context.WithCancel(context.Background())
	go cancel()
	if _, err := client.downloadStorageObject(ctx, testReportsBucket, "stats/installs/x.csv"); err == nil {
		t.Error("a cancelled download should fail rather than hang")
	}
}

// TestDownloadStorageObjectBoundsTheBodyItself: the listed size is checked
// before the transfer, but it is metadata. An object replaced between the two
// calls, or a bucket that reports no size, would otherwise arrive unbounded.
func TestDownloadStorageObjectBoundsTheBodyItself(t *testing.T) {
	original := maxReportBytes
	maxReportBytes = 32
	t.Cleanup(func() { maxReportBytes = original })

	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Larger than the bound, and the metadata never said so.
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	})
	client := newTestClient(t, api)

	_, err := client.downloadStorageObject(context.Background(), testReportsBucket, "stats/installs/x.csv")
	if err == nil {
		t.Fatal("expected an over-long body to be refused")
	}
	if !strings.Contains(err.Error(), "into memory") {
		t.Errorf("error = %v", err)
	}
	// Refused, not truncated: half a CSV parses as a whole one.
	if strings.Contains(err.Error(), "parse") {
		t.Errorf("the body should not have reached the parser: %v", err)
	}
}
