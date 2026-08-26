package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// User reviews. Two limits shape every tool here, and both are stated in the
// tool descriptions because an agent that does not know them will read an empty
// result as "no complaints":
//
//   - the API returns only reviews from the last week, and
//   - only for apps that have a live production release.
//
// Replies are not edit-scoped, so they take effect the moment they are sent —
// which is exactly why they go through the confirm flow.

// maxReplyRunes is Play's limit on a developer reply.
const maxReplyRunes = 350

// Review is one user review, flattened from the API's comments[] array into the
// shape a human or an agent actually reads.
type Review struct {
	ReviewID       string `json:"review_id"`
	AuthorName     string `json:"author_name,omitempty"`
	Stars          int    `json:"stars"`
	Text           string `json:"text,omitempty"`
	Language       string `json:"language,omitempty"`
	Device         string `json:"device,omitempty"`
	AndroidVersion int    `json:"android_os_version,omitempty"`
	AppVersionCode int64  `json:"app_version_code,omitempty"`
	AppVersionName string `json:"app_version_name,omitempty"`
	LastModified   string `json:"last_modified,omitempty"`
	// DeveloperReply is the existing reply, when there is one. Its presence is
	// the difference between "nobody has answered this" and "we already did".
	DeveloperReply     string          `json:"developer_reply,omitempty"`
	DeveloperReplyTime string          `json:"developer_reply_time,omitempty"`
	Raw                json.RawMessage `json:"raw,omitempty"`

	lastModified time.Time
}

// ReviewsArgs lists reviews.
type ReviewsArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Max         int    `json:"max,omitempty" jsonschema:"maximum number of reviews to return; defaults to 50"`
	Translate   string `json:"translate,omitempty" jsonschema:"a BCP-47 language to translate review text into, for example en"`
	Since       string `json:"since,omitempty" jsonschema:"only reviews modified within this window, for example 24h or 7d; the API itself only returns the last week"`
	MinStars    int    `json:"min_stars,omitempty" jsonschema:"only reviews with at least this star rating, 1 to 5"`
	MaxStars    int    `json:"max_stars,omitempty" jsonschema:"only reviews with at most this star rating, 1 to 5"`
	VersionCode int64  `json:"version_code,omitempty" jsonschema:"only reviews left on this app version code"`
	Unanswered  bool   `json:"unanswered,omitempty" jsonschema:"only reviews that have no developer reply yet"`
}

// ReviewsResult is the filtered review list.
type ReviewsResult struct {
	PackageName string   `json:"package_name"`
	Reviews     []Review `json:"reviews"`
	// Truncated says the walk stopped at the page cap rather than at the end.
	Truncated bool `json:"truncated,omitempty"`
	// Note carries the API's own scope limits, so an empty result is not read
	// as "no complaints".
	Note string `json:"note,omitempty"`
}

func (r ReviewsResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Reviews), []string{"review_id", "stars", "app_version_name", "last_modified", "text"}
}

// runReviews lists reviews, applying the filters the API does not offer.
//
// Only maxResults, the page token, and the translation language are server-side;
// star rating, version, age, and answered-ness are filtered here. That means the
// walk has to fetch more than `max` when filters are set, which is why the page
// cap and its truncation flag matter.
func runReviews(ctx context.Context, c *Client, args ReviewsArgs) (ReviewsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ReviewsResult{}, err
	}
	if err := validateStarRange(args.MinStars, args.MaxStars); err != nil {
		return ReviewsResult{}, err
	}
	var since time.Time
	if args.Since != "" {
		window, err := parseWindow(args.Since)
		if err != nil {
			return ReviewsResult{}, err
		}
		since = time.Now().Add(-window)
	}
	max := args.Max
	if max <= 0 {
		max = 50
	}

	out := ReviewsResult{
		PackageName: pkg,
		Note:        "Play returns reviews from the last week only, and only for apps with a live production release.",
	}
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		query.Set("maxResults", strconv.Itoa(min(max*2, 100)))
		if args.Translate != "" {
			query.Set("translationLanguage", args.Translate)
		}
		if token != "" {
			query.Set("token", token)
		}
		var page struct {
			Reviews         []json.RawMessage `json:"reviews"`
			TokenPagination struct {
				NextPageToken string `json:"nextPageToken"`
			} `json:"tokenPagination"`
		}
		if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/reviews", query, nil, &page); err != nil {
			return "", false, err
		}
		for _, raw := range page.Reviews {
			review := normalizeReview(raw)
			if !matchesReviewFilters(review, args, since) {
				continue
			}
			out.Reviews = append(out.Reviews, review)
			if len(out.Reviews) >= max {
				return "", false, nil
			}
		}
		return page.TokenPagination.NextPageToken, true, nil
	})
	if err != nil {
		return ReviewsResult{}, toolError("reviews", err)
	}
	out.Truncated = truncated
	return out, nil
}

// apiReview is the wire shape: a review is an author plus an array of comments,
// where the user's and the developer's are distinguished by which field is set.
type apiReview struct {
	ReviewID   string `json:"reviewId"`
	AuthorName string `json:"authorName"`
	Comments   []struct {
		UserComment *struct {
			Text             string        `json:"text"`
			LastModified     *apiTimestamp `json:"lastModified"`
			StarRating       int           `json:"starRating"`
			ReviewerLanguage string        `json:"reviewerLanguage"`
			Device           string        `json:"device"`
			AndroidOsVersion int           `json:"androidOsVersion"`
			AppVersionCode   int64         `json:"appVersionCode"`
			AppVersionName   string        `json:"appVersionName"`
		} `json:"userComment"`
		DeveloperComment *struct {
			Text         string        `json:"text"`
			LastModified *apiTimestamp `json:"lastModified"`
		} `json:"developerComment"`
	} `json:"comments"`
}

// apiTimestamp is Play's seconds/nanos timestamp, sent as strings.
type apiTimestamp struct {
	Seconds string `json:"seconds"`
	Nanos   int64  `json:"nanos"`
}

func (t *apiTimestamp) time() time.Time {
	if t == nil {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(t.Seconds, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, t.Nanos).UTC()
}

// normalizeReview flattens the comments array. The raw object is kept so
// nothing this binary does not model is lost.
func normalizeReview(raw json.RawMessage) Review {
	review := Review{Raw: raw}
	var r apiReview
	if err := json.Unmarshal(raw, &r); err != nil {
		return review
	}
	review.ReviewID, review.AuthorName = r.ReviewID, r.AuthorName
	for _, comment := range r.Comments {
		if c := comment.UserComment; c != nil {
			review.Text, review.Stars = c.Text, c.StarRating
			review.Language, review.Device = c.ReviewerLanguage, c.Device
			review.AndroidVersion = c.AndroidOsVersion
			review.AppVersionCode, review.AppVersionName = c.AppVersionCode, c.AppVersionName
			review.lastModified = c.LastModified.time()
			if !review.lastModified.IsZero() {
				review.LastModified = review.lastModified.Format(time.RFC3339)
			}
		}
		if c := comment.DeveloperComment; c != nil {
			review.DeveloperReply = c.Text
			if at := c.LastModified.time(); !at.IsZero() {
				review.DeveloperReplyTime = at.Format(time.RFC3339)
			}
		}
	}
	return review
}

// matchesReviewFilters applies the filters the API does not offer.
func matchesReviewFilters(r Review, args ReviewsArgs, since time.Time) bool {
	if args.MinStars > 0 && r.Stars < args.MinStars {
		return false
	}
	if args.MaxStars > 0 && r.Stars > args.MaxStars {
		return false
	}
	if args.VersionCode > 0 && r.AppVersionCode != args.VersionCode {
		return false
	}
	if args.Unanswered && r.DeveloperReply != "" {
		return false
	}
	// A review with no usable timestamp is kept: dropping it would hide a
	// complaint because the API sent a shape we did not expect.
	if !since.IsZero() && !r.lastModified.IsZero() && r.lastModified.Before(since) {
		return false
	}
	return true
}

func validateStarRange(minStars, maxStars int) error {
	for _, v := range []int{minStars, maxStars} {
		if v != 0 && (v < 1 || v > 5) {
			return fmt.Errorf("star ratings run from 1 to 5; got %d", v)
		}
	}
	if minStars > 0 && maxStars > 0 && minStars > maxStars {
		return fmt.Errorf("--min-stars %d is above --max-stars %d", minStars, maxStars)
	}
	return nil
}

// parseWindow accepts the durations people actually type for a review window:
// Go's own syntax plus a day suffix, which time.ParseDuration does not support.
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid window %q — use a duration such as 24h or 7d", s)
	}
	return d, nil
}

// --- a single review ---

// ReviewArgs reads one review.
type ReviewArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ReviewID    string `json:"review_id" jsonschema:"the review id, from play_reviews"`
	Translate   string `json:"translate,omitempty" jsonschema:"a BCP-47 language to translate the review text into"`
}

// ReviewResult is one review.
type ReviewResult struct {
	PackageName string `json:"package_name"`
	Review      Review `json:"review"`
}

func (r ReviewResult) tableRows() ([]json.RawMessage, []string) {
	return []json.RawMessage{jsonRow(r.Review)}, []string{"review_id", "stars", "app_version_name", "text", "developer_reply"}
}

// runReview reads one review by id.
func runReview(ctx context.Context, c *Client, args ReviewArgs) (ReviewResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ReviewResult{}, err
	}
	if args.ReviewID == "" {
		return ReviewResult{}, fmt.Errorf("review_id is required — pass --id (see `rollout play reviews`)")
	}
	query := url.Values{}
	if args.Translate != "" {
		query.Set("translationLanguage", args.Translate)
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/reviews/"+args.ReviewID, query, nil, &raw); err != nil {
		return ReviewResult{}, toolError("review", err)
	}
	return ReviewResult{PackageName: pkg, Review: normalizeReview(raw)}, nil
}

// --- replying ---

// ReplyReviewArgs replies to a review.
type ReplyReviewArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ReviewID    string `json:"review_id" jsonschema:"the review id to reply to, from play_reviews"`
	Text        string `json:"text" jsonschema:"the reply, at most 350 characters; it is public on the store page"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runReplyReview stages or applies a public reply.
//
// The preview quotes the review being answered. That is the point of the
// confirm step here: a reply is public, immediate, and attached to a specific
// user's complaint, and the failure mode worth preventing is answering the
// wrong one — which an id alone does nothing to catch.
func runReplyReview(ctx context.Context, c *Client, args ReplyReviewArgs) (WriteResult, error) {
	const tool = "reply_review"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.ReviewID == "" {
		return WriteResult{}, fmt.Errorf("review_id is required — pass --id (see `rollout play reviews`)")
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return WriteResult{}, fmt.Errorf("text is required — pass --text \"…\"")
	}
	if n := utf8.RuneCountInString(text); n > maxReplyRunes {
		return WriteResult{}, fmt.Errorf("the reply is %d characters; Play's limit is %d", n, maxReplyRunes)
	}

	current, err := runReview(ctx, c, ReviewArgs{PackageName: pkg, ReviewID: args.ReviewID})
	if err != nil {
		return WriteResult{}, err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Reply publicly to review %s", args.ReviewID)
	if current.Review.AuthorName != "" {
		fmt.Fprintf(&summary, " by %s", current.Review.AuthorName)
	}
	fmt.Fprintf(&summary, " (%d★): %s\n", current.Review.Stars, truncate(current.Review.Text, 200))
	if current.Review.DeveloperReply != "" {
		// Replying again replaces the existing answer rather than adding to it.
		fmt.Fprintf(&summary, "This review already has a reply, which will be replaced: %s\n", truncate(current.Review.DeveloperReply, 120))
	}
	fmt.Fprintf(&summary, "Your reply: %s", truncate(text, 350))

	body, err := json.Marshal(map[string]string{"replyText": text})
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect, Summary: summary.String(),
		Payload: editPayload{Requests: []editRequest{{
			Method:   http.MethodPost,
			Path:     "applications/" + pkg + "/reviews/" + args.ReviewID + ":reply",
			Body:     body,
			Describe: "reply to review " + args.ReviewID,
		}}},
	})
}

// --- CLI front-end ---

var (
	reviewsArgs   ReviewsArgs
	reviewsFormat string
	reviewArgs    ReviewArgs
	reviewFormat  string
	replyArgs     ReplyReviewArgs
)

var reviewsCmd = &cobra.Command{
	Use:         "reviews",
	Short:       "List user reviews (the API covers the last week only)",
	Annotations: mcpTool("reviews"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, reviewsArgs, reviewsFormat, runReviews)
	},
}

var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Read and reply to a single review",
}

var reviewGetCmd = &cobra.Command{
	Use:         "get",
	Short:       "Read one review",
	Annotations: mcpTool("review"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, reviewArgs, reviewFormat, runReview)
	},
}

var reviewReplyCmd = &cobra.Command{
	Use:         "reply",
	Short:       "Reply publicly to a review (previews the review first; --confirm to apply)",
	Annotations: mcpTool("reply_review"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, replyArgs, runReplyReview)
	},
}

func init() {
	addPackageFlag(reviewsCmd, &reviewsArgs.PackageName)
	reviewsCmd.Flags().IntVar(&reviewsArgs.Max, "max", 50, "maximum number of reviews to return")
	reviewsCmd.Flags().StringVar(&reviewsArgs.Translate, "translate", "", "translate review text into this language")
	reviewsCmd.Flags().StringVar(&reviewsArgs.Since, "since", "", "only reviews modified within this window (24h, 7d)")
	reviewsCmd.Flags().IntVar(&reviewsArgs.MinStars, "min-stars", 0, "only reviews with at least this rating")
	reviewsCmd.Flags().IntVar(&reviewsArgs.MaxStars, "max-stars", 0, "only reviews with at most this rating")
	reviewsCmd.Flags().Int64Var(&reviewsArgs.VersionCode, "version-code", 0, "only reviews left on this version code")
	reviewsCmd.Flags().BoolVar(&reviewsArgs.Unanswered, "unanswered", false, "only reviews with no developer reply")
	addFormatFlag(reviewsCmd, &reviewsFormat)

	addPackageFlag(reviewGetCmd, &reviewArgs.PackageName)
	reviewGetCmd.Flags().StringVar(&reviewArgs.ReviewID, "id", "", "review id (required)")
	reviewGetCmd.Flags().StringVar(&reviewArgs.Translate, "translate", "", "translate review text into this language")
	addFormatFlag(reviewGetCmd, &reviewFormat)

	addPackageFlag(reviewReplyCmd, &replyArgs.PackageName)
	reviewReplyCmd.Flags().StringVar(&replyArgs.ReviewID, "id", "", "review id (required)")
	reviewReplyCmd.Flags().StringVar(&replyArgs.Text, "text", "", "the reply text (max 350 characters)")
	addConfirmFlag(reviewReplyCmd, &replyArgs.Confirm)

	reviewCmd.AddCommand(reviewGetCmd, reviewReplyCmd)
}
