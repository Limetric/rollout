package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// usersAPI answers the users.list call every users tool makes, and records the
// writes an apply performs.
//
// It enforces the one thing a permissive fake would let through: both User and
// Grant mark `name` as Required, so a body without it is rejected by the real
// API even though the URL already carries every identifying part.
type usersAPI struct {
	list string
	// failGrants makes every grant POST fail, which is how the half-applied
	// invite is exercised.
	failGrants bool

	writes []recordedRequest
}

func (a *usersAPI) handler(t *testing.T) *fakePlayAPI {
	t.Helper()
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users") {
			writeJSON(w, http.StatusOK, a.list)
			return
		}
		body, _ := io.ReadAll(r.Body)
		a.writes = append(a.writes, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body),
		})
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			var sent struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(body, &sent); err != nil || sent.Name == "" {
				writeJSON(w, http.StatusBadRequest, `{"error":{"code":400,"message":"name is required","status":"INVALID_ARGUMENT"}}`)
				return
			}
		}
		if a.failGrants && strings.Contains(r.URL.Path, "/grants") && r.Method == http.MethodPost {
			writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"No access to com.example.app","status":"PERMISSION_DENIED"}}`)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
}

// newUsersTestClient is newTestClient plus the developer account id the users
// tools resolve, which nothing else needs.
func newUsersTestClient(t *testing.T, api *fakePlayAPI) *Client {
	t.Helper()
	clearPlayEnv(t)
	cfg := &PlayConfig{PackageName: "com.example.app", DeveloperID: "1234567890"}
	cfg.BaseURL = api.URL
	cfg.ReportingBaseURL = api.URL
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// twoUsers is the account most tests read: one full member with a grant, one
// outstanding invitation.
const twoUsers = `{"users":[
	{"name":"developers/1234567890/users/Dev@Example.com","email":"Dev@Example.com","accessState":"ACCESS_GRANTED",
	 "developerAccountPermissions":["CAN_VIEW_APP_QUALITY_GLOBAL"],
	 "grants":[{"name":"developers/1234567890/users/Dev@Example.com/grants/com.example.app","appLevelPermissions":["CAN_MANAGE_TRACK_APKS","CAN_VIEW_APP_QUALITY"]}]},
	{"name":"developers/1234567890/users/new@example.com","email":"new@example.com","accessState":"INVITED","expirationTime":"2026-01-31T00:00:00Z"}
]}`

func TestRunUsersReadsTheAccount(t *testing.T) {
	api := (&usersAPI{list: twoUsers}).handler(t)
	client := newUsersTestClient(t, api)

	res, err := runUsers(context.Background(), client, UsersArgs{})
	if err != nil {
		t.Fatalf("runUsers: %v", err)
	}
	if res.DeveloperID != "1234567890" || len(res.Users) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The package name lives in the resource name, and Play does not always
	// repeat it in its own field — a grant that reports no app is a grant
	// nobody can act on.
	if got := res.Users[0].Grants[0].PackageName; got != "com.example.app" {
		t.Errorf("grant package = %q, want it recovered from the resource name", got)
	}
	// An outstanding invitation looks exactly like a member until accessState
	// is read.
	if res.Users[1].AccessState != "INVITED" {
		t.Errorf("access state = %q", res.Users[1].AccessState)
	}
	if res.Users[1].ExpirationTime != "2026-01-31T00:00:00Z" {
		t.Errorf("expiration = %q", res.Users[1].ExpirationTime)
	}
}

func TestRunUsersFiltersByEmailIgnoringCase(t *testing.T) {
	api := (&usersAPI{list: twoUsers}).handler(t)
	client := newUsersTestClient(t, api)

	// Play echoes whatever case the invitation used, and nobody remembers it.
	res, err := runUsers(context.Background(), client, UsersArgs{Email: "dev@example.com"})
	if err != nil {
		t.Fatalf("runUsers: %v", err)
	}
	if len(res.Users) != 1 || res.Users[0].Email != "Dev@Example.com" {
		t.Fatalf("filter returned %+v", res.Users)
	}
}

// TestRunUsersNeedsADeveloperID: the id is the one setting nothing else uses,
// so the error has to say where it comes from.
func TestRunUsersNeedsADeveloperID(t *testing.T) {
	api := (&usersAPI{list: twoUsers}).handler(t)
	client := newTestClient(t, api)

	_, err := runUsers(context.Background(), client, UsersArgs{})
	if err == nil {
		t.Fatal("expected an error without a developer id")
	}
	if !strings.Contains(err.Error(), "set-developer-id") {
		t.Errorf("error should name the fix: %v", err)
	}
}

func TestRunUsersPagesThroughEveryUser(t *testing.T) {
	page := 0
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			writeJSON(w, http.StatusOK, `{"users":[{"email":"a@example.com"}],"tokenPagination":{"nextPageToken":"p2"}}`)
			return
		}
		if r.URL.Query().Get("pageToken") != "p2" {
			t.Errorf("second call did not carry the page token: %q", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, `{"users":[{"email":"b@example.com"}]}`)
	})
	client := newUsersTestClient(t, api)

	res, err := runUsers(context.Background(), client, UsersArgs{})
	if err != nil {
		t.Fatalf("runUsers: %v", err)
	}
	if len(res.Users) != 2 {
		t.Fatalf("read %d users across pages, want 2", len(res.Users))
	}
}

// TestPermissionFromTheWrongListNamesTheRightFlag: the two enums differ only by
// a _GLOBAL suffix, and Play's own rejection quotes neither the value nor the
// alternative.
func TestPermissionFromTheWrongListNamesTheRightFlag(t *testing.T) {
	_, err := normalizeAppPermissions([]string{"CAN_MANAGE_TRACK_APKS_GLOBAL"})
	if err == nil {
		t.Fatal("expected an account-level permission to be refused as an app-level one")
	}
	if !strings.Contains(err.Error(), "--permissions") {
		t.Errorf("error should name the flag that takes it: %v", err)
	}

	_, err = normalizeDeveloperPermissions([]string{"CAN_MANAGE_TRACK_APKS"})
	if err == nil || !strings.Contains(err.Error(), "--app-permissions") {
		t.Errorf("app-level value in the account list should point at --app-permissions: %v", err)
	}

	// Unknown values list what is accepted, rather than leaving the caller to
	// find the enum in Google's reference.
	_, err = normalizeAppPermissions([]string{"can_do_anything"})
	if err == nil || !strings.Contains(err.Error(), "CAN_MANAGE_TRACK_APKS") {
		t.Errorf("unknown permission should list the alternatives: %v", err)
	}

	// Case and comma-separation are accepted: an agent pastes what it read.
	got, err := normalizeAppPermissions([]string{"can_manage_track_apks, can_view_app_quality"})
	if err != nil {
		t.Fatalf("normalizeAppPermissions: %v", err)
	}
	if len(got) != 2 || got[0] != "CAN_MANAGE_TRACK_APKS" {
		t.Errorf("normalized to %v", got)
	}
}

func TestInviteUserCreatesTheUserThenEachGrant(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runInviteUser(ctx, client, InviteUserArgs{
		Email:          "fresh@example.com",
		Permissions:    []string{"CAN_VIEW_APP_QUALITY_GLOBAL"},
		Apps:           []string{"com.example.app"},
		AppPermissions: []string{"CAN_MANAGE_TRACK_APKS"},
		Expires:        "2026-06-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("runInviteUser: %v", err)
	}
	if preview.Applied || preview.ConfirmToken == "" {
		t.Fatalf("first call must not apply: %+v", preview)
	}
	// The preview has to name the exact enums: they are the arguments nobody
	// can check by reading them back.
	for _, want := range []string{"CAN_VIEW_APP_QUALITY_GLOBAL", "CAN_MANAGE_TRACK_APKS", "2026-06-01T00:00:00Z"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview does not mention %s:\n%s", want, preview.Preview)
		}
	}

	applyPreview(t, ctx, client, "invite_user", preview.ConfirmToken)

	if len(backend.writes) != 2 {
		t.Fatalf("wrote %d requests, want the user then the grant: %+v", len(backend.writes), backend.writes)
	}
	if backend.writes[0].Path != "/androidpublisher/v3/developers/1234567890/users" {
		t.Errorf("user was created at %q", backend.writes[0].Path)
	}
	if !strings.HasSuffix(backend.writes[1].Path, "/users/fresh@example.com/grants") {
		t.Errorf("grant was created at %q", backend.writes[1].Path)
	}
}

// TestInviteUserRefusesAnEmptyInvitation: Play accepts an invitation that
// permits nothing and says nothing about it.
// TestUserAndGrantBodiesCarryTheirResourceName: both User and Grant mark `name`
// as Required, and the API rejects a body without one even though the URL
// already carries every identifying part. The name is the resource name itself,
// so it is spelled raw — unlike the URL, whose segments are escaped.
func TestUserAndGrantBodiesCarryTheirResourceName(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runInviteUser(ctx, client, InviteUserArgs{
		Email: "fresh@example.com", Apps: []string{"com.example.app"},
		AppPermissions: []string{"CAN_MANAGE_TRACK_APKS"},
	})
	if err != nil {
		t.Fatalf("runInviteUser: %v", err)
	}
	applyPreview(t, ctx, client, "invite_user", preview.ConfirmToken)

	if len(backend.writes) != 2 {
		t.Fatalf("wrote %+v", backend.writes)
	}
	assertResourceName(t, backend.writes[0].Body, "developers/1234567890/users/fresh@example.com")
	assertResourceName(t, backend.writes[1].Body, "developers/1234567890/users/fresh@example.com/grants/com.example.app")

	// A patch names the grant too, and matches the case Play holds the user
	// under rather than the case the caller typed.
	backend.writes = nil
	preview, err = runSetGrant(ctx, client, SetGrantArgs{
		Email: "dev@example.com", Permissions: []string{"CAN_MANAGE_TRACK_APKS"},
	})
	if err != nil {
		t.Fatalf("runSetGrant: %v", err)
	}
	applyPreview(t, ctx, client, "set_grant", preview.ConfirmToken)
	assertResourceName(t, backend.writes[0].Body, "developers/1234567890/users/Dev@Example.com/grants/com.example.app")
}

func assertResourceName(t *testing.T, body, want string) {
	t.Helper()
	var sent struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if sent.Name != want {
		t.Errorf("name = %q, want %q", sent.Name, want)
	}
}

func TestInviteUserRefusesAnEmptyInvitation(t *testing.T) {
	client := newUsersTestClient(t, (&usersAPI{list: twoUsers}).handler(t))

	_, err := runInviteUser(context.Background(), client, InviteUserArgs{Email: "fresh@example.com"})
	if err == nil || !strings.Contains(err.Error(), "grants nothing") {
		t.Errorf("expected an empty invitation to be refused: %v", err)
	}

	_, err = runInviteUser(context.Background(), client, InviteUserArgs{
		Email: "fresh@example.com", Apps: []string{"com.example.app"},
	})
	if err == nil || !strings.Contains(err.Error(), "--app-permissions") {
		t.Errorf("apps without permissions should name the missing flag: %v", err)
	}
}

// TestInviteUserRefusesAnExistingMember: users.create fails on a duplicate with
// a message that does not suggest the command that would work.
func TestInviteUserRefusesAnExistingMember(t *testing.T) {
	client := newUsersTestClient(t, (&usersAPI{list: twoUsers}).handler(t))

	_, err := runInviteUser(context.Background(), client, InviteUserArgs{
		Email: "dev@example.com", Permissions: []string{"CAN_VIEW_APP_QUALITY_GLOBAL"},
	})
	if err == nil || !strings.Contains(err.Error(), "user grant") {
		t.Errorf("expected the existing member to be refused with a pointer at `user grant`: %v", err)
	}
}

// TestInviteUserReportsWhatAlreadyLanded: users and grants are separate calls
// with no transaction across them, so a failure part-way has really created the
// user — and re-running the command would fail on the duplicate.
func TestInviteUserReportsWhatAlreadyLanded(t *testing.T) {
	backend := &usersAPI{list: twoUsers, failGrants: true}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runInviteUser(ctx, client, InviteUserArgs{
		Email: "fresh@example.com", Apps: []string{"com.example.app"},
		AppPermissions: []string{"CAN_MANAGE_TRACK_APKS"},
	})
	if err != nil {
		t.Fatalf("runInviteUser: %v", err)
	}
	_, err = applyConfirmed(ctx, client, "invite_user", preview.ConfirmToken)
	if err == nil {
		t.Fatal("expected the failing grant to surface")
	}
	if !strings.Contains(err.Error(), "grant on com.example.app") {
		t.Errorf("error should name the failing step: %v", err)
	}
	if !strings.Contains(err.Error(), "invite fresh@example.com") {
		t.Errorf("error should name what already applied: %v", err)
	}
}

func TestSetGrantPatchesAnExistingGrantWithAnUpdateMask(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runSetGrant(ctx, client, SetGrantArgs{
		Email: "dev@example.com", Permissions: []string{"CAN_MANAGE_TRACK_APKS"},
	})
	if err != nil {
		t.Fatalf("runSetGrant: %v", err)
	}
	// A full replacement that silently drops the permission you did not retype
	// is exactly what the diff exists to show.
	if !strings.Contains(preview.Preview, "revoking CAN_VIEW_APP_QUALITY") {
		t.Errorf("preview does not show what the replacement drops:\n%s", preview.Preview)
	}

	applyPreview(t, ctx, client, "set_grant", preview.ConfirmToken)

	if len(backend.writes) != 1 {
		t.Fatalf("wrote %+v", backend.writes)
	}
	write := backend.writes[0]
	if write.Method != http.MethodPatch {
		t.Errorf("existing grant should be patched, got %s", write.Method)
	}
	// Without the mask the API treats absent fields as cleared.
	if !strings.Contains(write.Query, "updateMask=appLevelPermissions") {
		t.Errorf("patch query = %q", write.Query)
	}
	// The email is the case Play holds, not the case the caller typed: the
	// address is a path segment.
	if !strings.Contains(write.Path, "/users/Dev@Example.com/grants/com.example.app") {
		t.Errorf("patched %q", write.Path)
	}
}

func TestSetGrantCreatesAGrantTheUserDoesNotHave(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	// The API has no upsert: patching a grant that does not exist fails.
	preview, err := runSetGrant(ctx, client, SetGrantArgs{
		Email: "dev@example.com", PackageName: "com.example.other",
		Permissions: []string{"CAN_VIEW_APP_QUALITY"},
	})
	if err != nil {
		t.Fatalf("runSetGrant: %v", err)
	}
	applyPreview(t, ctx, client, "set_grant", preview.ConfirmToken)

	if backend.writes[0].Method != http.MethodPost {
		t.Errorf("a new grant should be created, got %s %s", backend.writes[0].Method, backend.writes[0].Path)
	}
}

func TestSetGrantRefusesAnUnknownUser(t *testing.T) {
	client := newUsersTestClient(t, (&usersAPI{list: twoUsers}).handler(t))

	_, err := runSetGrant(context.Background(), client, SetGrantArgs{
		Email: "stranger@example.com", Permissions: []string{"CAN_VIEW_APP_QUALITY"},
	})
	if err == nil || !strings.Contains(err.Error(), "user invite") {
		t.Errorf("expected an unknown user to point at the invite command: %v", err)
	}
}

// TestRevokeGrantTakesTwoConfirmations: the API cannot report afterwards what
// the grant held, so the preview is the only record and confirming it must be
// deliberate.
func TestRevokeGrantTakesTwoConfirmations(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runRevokeGrant(ctx, client, RevokeGrantArgs{Email: "dev@example.com"})
	if err != nil {
		t.Fatalf("runRevokeGrant: %v", err)
	}
	if !strings.Contains(preview.Preview, "CAN_MANAGE_TRACK_APKS") {
		t.Errorf("preview must name what is being taken away:\n%s", preview.Preview)
	}
	// Account-level access survives a per-app revoke, and saying so is what
	// stops a second, unnecessary removal.
	if !strings.Contains(preview.Preview, "account-level permissions") {
		t.Errorf("preview should say what the user keeps:\n%s", preview.Preview)
	}

	first, err := applyConfirmed(ctx, client, "revoke_grant", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied || first.ConfirmToken == "" {
		t.Fatalf("first confirm must re-stage rather than apply: %+v", first)
	}
	if len(backend.writes) != 0 {
		t.Fatalf("nothing may be sent before the second confirmation: %+v", backend.writes)
	}

	applyPreview(t, ctx, client, "revoke_grant", first.ConfirmToken)
	if len(backend.writes) != 1 || backend.writes[0].Method != http.MethodDelete {
		t.Fatalf("wrote %+v", backend.writes)
	}
	if !strings.HasSuffix(backend.writes[0].Path, "/grants/com.example.app") {
		t.Errorf("deleted %q", backend.writes[0].Path)
	}
}

func TestRevokeGrantRefusesWhenThereIsNothingToRevoke(t *testing.T) {
	client := newUsersTestClient(t, (&usersAPI{list: twoUsers}).handler(t))

	_, err := runRevokeGrant(context.Background(), client, RevokeGrantArgs{
		Email: "dev@example.com", PackageName: "com.example.other",
	})
	if err == nil || !strings.Contains(err.Error(), "no grant") {
		t.Errorf("expected a missing grant to be refused: %v", err)
	}
}

// TestRemoveUserPreviewIsTheOnlyRecord: removing a user drops every grant at
// once, and nothing in the API can list them afterwards.
func TestRemoveUserPreviewIsTheOnlyRecord(t *testing.T) {
	backend := &usersAPI{list: twoUsers}
	client := newUsersTestClient(t, backend.handler(t))
	ctx := context.Background()

	preview, err := runRemoveUser(ctx, client, RemoveUserArgs{Email: "dev@example.com"})
	if err != nil {
		t.Fatalf("runRemoveUser: %v", err)
	}
	for _, want := range []string{"CAN_VIEW_APP_QUALITY_GLOBAL", "com.example.app", "CAN_MANAGE_TRACK_APKS"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview does not record %s:\n%s", want, preview.Preview)
		}
	}

	first, err := applyConfirmed(ctx, client, "remove_user", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("removing a user must take two confirmations")
	}
	applyPreview(t, ctx, client, "remove_user", first.ConfirmToken)

	if len(backend.writes) != 1 || backend.writes[0].Method != http.MethodDelete {
		t.Fatalf("wrote %+v", backend.writes)
	}
	if !strings.HasSuffix(backend.writes[0].Path, "/users/Dev@Example.com") {
		t.Errorf("deleted %q", backend.writes[0].Path)
	}
}

// TestUsersWritesAreStampedForTheAudit: a developer-account write has no
// package, and an audit line naming nothing is not worth writing.
func TestUsersWritesAreStampedForTheAudit(t *testing.T) {
	client := newUsersTestClient(t, (&usersAPI{list: twoUsers}).handler(t))

	preview, err := runRemoveUser(context.Background(), client, RemoveUserArgs{Email: "dev@example.com"})
	if err != nil {
		t.Fatalf("runRemoveUser: %v", err)
	}
	p, err := peekMutation(preview.ConfirmToken)
	if err != nil {
		t.Fatalf("peekMutation: %v", err)
	}
	if p.PackageName != "developers/1234567890" {
		t.Errorf("staged package = %q, want the developer account", p.PackageName)
	}
	if p.Platform != playPlatformName {
		t.Errorf("staged platform = %q", p.Platform)
	}
}

func TestParseExpiryRejectsAnythingButRFC3339(t *testing.T) {
	if _, err := parseExpiry("next tuesday"); err == nil {
		t.Fatal("expected a bad timestamp to be refused")
	}
	got, err := parseExpiry("2026-06-01T12:00:00+02:00")
	if err != nil {
		t.Fatalf("parseExpiry: %v", err)
	}
	if got != "2026-06-01T10:00:00Z" {
		t.Errorf("expiry = %q, want it normalized to UTC", got)
	}
}

func TestUsersResultRendersATable(t *testing.T) {
	res := UsersResult{Users: []PlayUser{{
		Email: "dev@example.com", AccessState: "ACCESS_GRANTED",
		Grants: []UserGrant{{PackageName: "com.example.app"}, {PackageName: "com.example.other"}},
	}}}
	rows, fields := res.tableRows()
	if len(rows) != 1 || len(fields) != 4 {
		t.Fatalf("rows=%d fields=%v", len(rows), fields)
	}
	var row map[string]string
	if err := json.Unmarshal(rows[0], &row); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	if row["apps"] != "com.example.app com.example.other" {
		t.Errorf("apps column = %q", row["apps"])
	}
}
