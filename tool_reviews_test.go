package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reviewJSON builds a review in the API's own shape.
func reviewJSON(id string, stars int, text string, versionCode int64, age time.Duration, reply string) string {
	seconds := time.Now().Add(-age).Unix()
	body := fmt.Sprintf(`{"reviewId":%q,"authorName":"A. User","comments":[
		{"userComment":{"text":%q,"lastModified":{"seconds":"%d","nanos":0},"starRating":%d,
		 "reviewerLanguage":"en","device":"Pixel 8","androidOsVersion":34,
		 "appVersionCode":%d,"appVersionName":"1.5.0"}}`,
		id, text, seconds, stars, versionCode)
	if reply != "" {
		body += fmt.Sprintf(`,{"developerComment":{"text":%q,"lastModified":{"seconds":"%d","nanos":0}}}`, reply, seconds)
	}
	return body + "]}"
}

// reviewsAPI serves a fixed set of reviews across two pages.
func reviewsAPI(t *testing.T, pages [][]string) *fakePlayAPI {
	t.Helper()
	var served int
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusOK, `{"result":{"replyText":"Thanks!"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/reviews/") {
			// A single review read; the id is the last path segment.
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-1]
			for _, page := range pages {
				for _, body := range page {
					if strings.Contains(body, `"`+id+`"`) {
						writeJSON(w, http.StatusOK, body)
						return
					}
				}
			}
			writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Review not found","status":"NOT_FOUND"}}`)
			return
		}
		page := pages[min(served, len(pages)-1)]
		served++
		body := `{"reviews":[` + strings.Join(page, ",") + `]`
		if served < len(pages) {
			body += `,"tokenPagination":{"nextPageToken":"p2"}`
		}
		writeJSON(w, http.StatusOK, body+"}")
	})
}

func TestRunReviewsPagesAndNormalizes(t *testing.T) {
	api := reviewsAPI(t, [][]string{
		{reviewJSON("r1", 1, "Crashes on launch", 42, time.Hour, "")},
		{reviewJSON("r2", 5, "Love it", 42, 2*time.Hour, "Thanks!")},
	})
	client := newTestClient(t, api)

	res, err := runReviews(context.Background(), client, ReviewsArgs{})
	if err != nil {
		t.Fatalf("runReviews: %v", err)
	}
	if len(res.Reviews) != 2 {
		t.Fatalf("got %d reviews across two pages: %+v", len(res.Reviews), res.Reviews)
	}

	first := res.Reviews[0]
	// The API nests the user's comment inside comments[]; flattening it is the
	// point of this tool.
	if first.ReviewID != "r1" || first.Stars != 1 || first.Text != "Crashes on launch" {
		t.Errorf("unexpected review: %+v", first)
	}
	if first.Device != "Pixel 8" || first.AppVersionName != "1.5.0" || first.AppVersionCode != 42 {
		t.Errorf("device or version metadata was lost: %+v", first)
	}
	if first.LastModified == "" {
		t.Errorf("the seconds/nanos timestamp was not rendered: %+v", first)
	}
	// An existing reply is the difference between "nobody answered" and "we
	// already did".
	if res.Reviews[1].DeveloperReply != "Thanks!" {
		t.Errorf("the developer reply was dropped: %+v", res.Reviews[1])
	}
	// The API's scope limits have to reach the caller, or an empty result reads
	// as "no complaints".
	if !strings.Contains(res.Note, "last week") || !strings.Contains(res.Note, "production release") {
		t.Errorf("note should state the API's limits: %q", res.Note)
	}

	// The second page must carry the token, or the walk repeats page one.
	if len(api.seen()) != 2 || !strings.Contains(api.seen()[1].Query, "token=p2") {
		t.Errorf("pagination token was not sent: %+v", api.seen())
	}
}

func TestReviewFilters(t *testing.T) {
	page := []string{
		reviewJSON("one-star", 1, "Bad", 42, time.Hour, ""),
		reviewJSON("three-star", 3, "Okay", 41, 10*24*time.Hour, ""),
		reviewJSON("five-star", 5, "Great", 42, time.Hour, "Thanks!"),
	}

	tests := []struct {
		name string
		args ReviewsArgs
		want []string
	}{
		{"no filters", ReviewsArgs{}, []string{"one-star", "three-star", "five-star"}},
		{"minimum stars", ReviewsArgs{MinStars: 3}, []string{"three-star", "five-star"}},
		{"maximum stars", ReviewsArgs{MaxStars: 3}, []string{"one-star", "three-star"}},
		{"a star range", ReviewsArgs{MinStars: 2, MaxStars: 4}, []string{"three-star"}},
		{"version code", ReviewsArgs{VersionCode: 41}, []string{"three-star"}},
		{"unanswered only", ReviewsArgs{Unanswered: true}, []string{"one-star", "three-star"}},
		{"recent only", ReviewsArgs{Since: "2d"}, []string{"one-star", "five-star"}},
		{"a cap", ReviewsArgs{Max: 1}, []string{"one-star"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := reviewsAPI(t, [][]string{page})
			client := newTestClient(t, api)

			res, err := runReviews(context.Background(), client, tc.args)
			if err != nil {
				t.Fatalf("runReviews: %v", err)
			}
			var got []string
			for _, review := range res.Reviews {
				got = append(got, review.ReviewID)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReviewFilterValidation(t *testing.T) {
	api := reviewsAPI(t, [][]string{{}})
	client := newTestClient(t, api)
	ctx := context.Background()

	for _, args := range []ReviewsArgs{
		{MinStars: 0, MaxStars: 6},
		{MinStars: -1},
		{MinStars: 4, MaxStars: 2},
	} {
		if _, err := runReviews(ctx, client, args); err == nil {
			t.Errorf("%+v should have been rejected", args)
		}
	}
	if _, err := runReviews(ctx, client, ReviewsArgs{Since: "last week"}); err == nil {
		t.Error("an unparseable window should be rejected")
	}
}

func TestParseWindow(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"90m", 90 * time.Minute},
	}
	for _, tc := range tests {
		got, err := parseWindow(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseWindow(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "0d", "-3h", "soon"} {
		if _, err := parseWindow(bad); err == nil {
			t.Errorf("parseWindow(%q) should have failed", bad)
		}
	}
}

// TestReviewWithoutATimestampIsKept: dropping it would hide a complaint because
// the API sent a shape we did not expect.
func TestReviewWithoutATimestampIsKept(t *testing.T) {
	api := reviewsAPI(t, [][]string{{
		`{"reviewId":"odd","comments":[{"userComment":{"text":"Hmm","starRating":2}}]}`,
	}})
	client := newTestClient(t, api)

	res, err := runReviews(context.Background(), client, ReviewsArgs{Since: "1h"})
	if err != nil {
		t.Fatalf("runReviews: %v", err)
	}
	if len(res.Reviews) != 1 {
		t.Errorf("a review with no timestamp was filtered out: %+v", res.Reviews)
	}
}

func TestRunReview(t *testing.T) {
	api := reviewsAPI(t, [][]string{{reviewJSON("r1", 2, "Slow", 42, time.Hour, "")}})
	client := newTestClient(t, api)

	res, err := runReview(context.Background(), client, ReviewArgs{ReviewID: "r1", Translate: "en"})
	if err != nil {
		t.Fatalf("runReview: %v", err)
	}
	if res.Review.ReviewID != "r1" || res.Review.Stars != 2 {
		t.Errorf("unexpected review: %+v", res.Review)
	}
	if !strings.Contains(api.seen()[0].Query, "translationLanguage=en") {
		t.Errorf("the translation language was not sent: %q", api.seen()[0].Query)
	}

	if _, err := runReview(context.Background(), client, ReviewArgs{}); err == nil {
		t.Error("review_id should be required")
	}
}

// TestReplyPreviewQuotesTheReview is the reason a reply goes through the
// confirm flow: a reply is public, immediate, and attached to one user's
// complaint, and an id alone does nothing to catch answering the wrong one.
func TestReplyPreviewQuotesTheReview(t *testing.T) {
	isolateState(t)
	api := reviewsAPI(t, [][]string{{reviewJSON("r1", 1, "Crashes on launch every time", 42, time.Hour, "")}})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runReplyReview(ctx, client, ReplyReviewArgs{ReviewID: "r1", Text: "Sorry — fixed in 1.5.1."})
	if err != nil {
		t.Fatalf("runReplyReview: %v", err)
	}
	for _, want := range []string{"r1", "A. User", "1★", "Crashes on launch every time", "Sorry — fixed in 1.5.1."} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	res, err := applyConfirmed(ctx, client, "reply_review", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !res.Applied {
		t.Fatal("expected the reply to apply")
	}
	// A reply is not edit-scoped; wrapping it in an edit would open and commit
	// an empty one.
	for _, call := range api.calls() {
		if strings.HasSuffix(call, "/edits") {
			t.Errorf("the reply opened an edit: %v", api.calls())
		}
	}
	if !api.sawCall(http.MethodPost, "/androidpublisher/v3/applications/com.example.app/reviews/r1:reply") {
		t.Errorf("the reply did not reach the API: %v", api.calls())
	}
}

// TestReplyWarnsAboutReplacingAnExistingReply: replying again replaces the
// answer rather than adding to it.
func TestReplyWarnsAboutReplacingAnExistingReply(t *testing.T) {
	isolateState(t)
	api := reviewsAPI(t, [][]string{{reviewJSON("r1", 3, "Meh", 42, time.Hour, "We hear you")}})
	client := newTestClient(t, api)

	preview, err := runReplyReview(context.Background(), client, ReplyReviewArgs{ReviewID: "r1", Text: "Try 1.5.1."})
	if err != nil {
		t.Fatalf("runReplyReview: %v", err)
	}
	if !strings.Contains(preview.Preview, "will be replaced") {
		t.Errorf("preview should warn about the existing reply:\n%s", preview.Preview)
	}
	if !strings.Contains(preview.Preview, "We hear you") {
		t.Errorf("preview should quote the existing reply:\n%s", preview.Preview)
	}
}

func TestReplyValidatesInput(t *testing.T) {
	isolateState(t)
	api := reviewsAPI(t, [][]string{{reviewJSON("r1", 3, "Meh", 42, time.Hour, "")}})
	client := newTestClient(t, api)
	ctx := context.Background()

	if _, err := runReplyReview(ctx, client, ReplyReviewArgs{Text: "hi"}); err == nil {
		t.Error("review_id should be required")
	}
	if _, err := runReplyReview(ctx, client, ReplyReviewArgs{ReviewID: "r1", Text: "   "}); err == nil {
		t.Error("an empty reply should be rejected")
	}
	_, err := runReplyReview(ctx, client, ReplyReviewArgs{ReviewID: "r1", Text: strings.Repeat("x", maxReplyRunes+1)})
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(maxReplyRunes)) {
		t.Errorf("err = %v, want one naming the 350-character limit", err)
	}
	// Runes, not bytes.
	if _, err := runReplyReview(ctx, client, ReplyReviewArgs{ReviewID: "r1", Text: strings.Repeat("あ", maxReplyRunes)}); err != nil {
		t.Errorf("a 350-rune reply should be accepted: %v", err)
	}
}

func TestReviewResultsRenderAsTables(t *testing.T) {
	results := []any{
		ReviewsResult{Reviews: []Review{{ReviewID: "r1", Stars: 3, Text: "Meh"}}},
		ReviewResult{Review: Review{ReviewID: "r1", Stars: 3}},
	}
	for _, res := range results {
		for _, format := range []string{"json", "table", "csv"} {
			var buf strings.Builder
			if err := printResult(&buf, format, res); err != nil {
				t.Errorf("%T as %s: %v", res, format, err)
			}
		}
	}
}

func TestReviewRawIsPreserved(t *testing.T) {
	// The raw object keeps whatever this binary does not model — device
	// metadata, thumbs-up counts, original untranslated text.
	raw := json.RawMessage(`{"reviewId":"r1","comments":[{"userComment":{"text":"Hi","starRating":4,"thumbsUpCount":7}}]}`)
	review := normalizeReview(raw)
	if review.Stars != 4 {
		t.Fatalf("unexpected review: %+v", review)
	}
	if !strings.Contains(string(review.Raw), "thumbsUpCount") {
		t.Errorf("raw review was dropped: %s", review.Raw)
	}
}
