package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Cloud Storage side of Play reporting.
//
// Installs, ratings, crash statistics, store performance, reviews and the
// financial reports have no API at all. Play exports them as CSV to a bucket
// named `pubsite_prod_rev_<developer id>` — the one Play Console → Download
// reports points at — and reading that bucket is the only way to get them.
//
// This is deliberately the smallest possible Cloud Storage client: list objects
// under a prefix, download one object. It reuses client.go's request plumbing,
// so a throttled or flaky bucket read backs off exactly like a Publisher call
// and a refused one is parsed from the same Google error envelope.

// storageAPIPath is the version-carrying prefix for Cloud Storage JSON calls.
const storageAPIPath = "storage/v1"

// storageObject is one object in the reports bucket, as objects.list returns
// it. Size is a string on the wire because a Cloud Storage object can exceed
// what JSON numbers represent exactly.
type storageObject struct {
	Name        string `json:"name"`
	Size        string `json:"size"`
	Updated     string `json:"updated"`
	ContentType string `json:"contentType"`
}

// sizeBytes parses Size, reporting 0 for the malformed case rather than
// failing a listing over a cosmetic field.
func (o storageObject) sizeBytes() int64 {
	n, err := strconv.ParseInt(o.Size, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// base is the object's file name without its folder prefix.
func (o storageObject) base() string {
	if i := strings.LastIndex(o.Name, "/"); i >= 0 {
		return o.Name[i+1:]
	}
	return o.Name
}

// bucketURL builds a Cloud Storage JSON API URL for one bucket's objects.
// The bucket name is escaped because it comes from configuration.
func (c *Client) bucketURL(bucket string) string {
	return c.cfg.StorageBaseURL + "/" + storageAPIPath + "/b/" + url.PathEscape(bucket) + "/o"
}

// reportsBucket resolves the configured export bucket, or explains how to set
// one. The name is not guessable from anything rollout already knows — it
// carries a developer-account-specific id — so there is no default to fall back
// to.
func (c *Client) reportsBucket() (string, error) {
	if bucket := strings.TrimSpace(c.cfg.ReportsBucket); bucket != "" {
		return bucket, nil
	}
	return "", fmt.Errorf("no reports bucket configured — run `rollout config play set-reports-bucket pubsite_prod_rev_…` or set PLAY_REPORTS_BUCKET; the name is on Play Console → Download reports, next to \"Cloud Storage URI\"")
}

// listStorageObjects lists every object under prefix, newest-first ordering
// left to the caller (Cloud Storage returns them lexicographically, which for
// these names is chronological anyway).
//
// It reports whether the listing stopped at maxPages rather than at the end of
// the bucket, so a caller can say the result is partial instead of presenting a
// truncated list as complete.
func (c *Client) listStorageObjects(ctx context.Context, bucket, prefix string) ([]storageObject, bool, error) {
	var objects []storageObject
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := url.Values{
			"prefix":     {prefix},
			"fields":     {"items(name,size,updated,contentType),nextPageToken"},
			"maxResults": {"1000"},
		}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			pagedResponse
			Items []storageObject `json:"items"`
		}
		if err := c.doAt(ctx, c.bucketURL(bucket), http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("list gs://%s/%s: %w", bucket, prefix, err)
		}
		objects = append(objects, page.Items...)
		return page.next(), true, nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Name < objects[j].Name })
	return objects, truncated, nil
}

// reportDownloadTimeout bounds one report download.
//
// It exists because the streaming client has no timeout of its own: the 60s cap
// that keeps an ordinary API call from hanging would abort a large export
// part-way on a slow link, and dropping the cap entirely would let a stalled
// response hang a CLI run forever. A CLI command runs on a background context,
// so this is the only deadline there is.
const reportDownloadTimeout = 10 * time.Minute

// statStorageObject reads one object's metadata without downloading it.
//
// The resolved path already knows an object's size from the listing it came
// from; this is how the `--object` path learns it, which is what lets an
// implausibly large report be refused before it is pulled into memory. It also
// turns a mistyped object name into a 404 before any transfer starts.
func (c *Client) statStorageObject(ctx context.Context, bucket, name string) (storageObject, error) {
	rawURL := c.bucketURL(bucket) + "/" + url.PathEscape(name)
	query := url.Values{"fields": {"name,size,updated,contentType"}}
	var obj storageObject
	if err := c.doAt(ctx, rawURL, http.MethodGet, query, nil, &obj, retryIdempotent); err != nil {
		return storageObject{}, fmt.Errorf("read gs://%s/%s: %w", bucket, name, err)
	}
	return obj, nil
}

// downloadStorageObject fetches one object's bytes.
//
// The object name is path-escaped whole — slashes included — because the JSON
// API takes it as a single path segment: `stats/installs/x.csv` has to arrive
// as `stats%2Finstalls%2Fx.csv` or the request addresses a different resource.
func (c *Client) downloadStorageObject(ctx context.Context, bucket, name string) ([]byte, error) {
	rawURL := c.bucketURL(bucket) + "/" + url.PathEscape(name)
	ctx, cancel := context.WithTimeout(ctx, reportDownloadTimeout)
	defer cancel()
	// The listed size is checked before we get here, but it is metadata: an
	// object replaced between the two calls, or a bucket that reports no size,
	// would otherwise arrive unbounded.
	data, err := c.fetch(ctx, rawURL, http.MethodGet, url.Values{"alt": {"media"}}, nil,
		fetchOptions{policy: retryIdempotent, accept: "*/*", client: c.stream, maxBytes: maxReportBytes})
	if err != nil {
		return nil, fmt.Errorf("read gs://%s/%s: %w", bucket, name, err)
	}
	return data, nil
}

// listReportObjects asks the reports bucket for a single object name, so
// `doctor` can prove the bucket is reachable — nothing local can. The
// devstorage scope is added to the credential only when a reports bucket is
// configured (see PlayConfig.scopes), so a user who signed in before setting
// one holds a token that predates the scope: a state the config looks perfectly
// right in and only a live call exposes.
//
// Listing is the operation every report read starts with, so it is the one
// worth proving; asking for bucket metadata instead would check a different
// permission from the one that matters.
func (c *Client) listReportObjects(ctx context.Context, bucket string) error {
	query := url.Values{"maxResults": {"1"}, "fields": {"items/name"}}
	var page struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.doAt(ctx, c.bucketURL(bucket), http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
		return fmt.Errorf("list gs://%s: %w", bucket, err)
	}
	return nil
}
