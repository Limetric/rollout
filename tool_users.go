package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Developer-account users and their per-app grants.
//
// This is the one Play surface that is not addressed by package name: users
// hang off the developer account, so every tool here resolves a developer ID
// (the number in the Play Console URL) instead of, or as well as, an app.

// UsersArgs lists the users on a developer account.
type UsersArgs struct {
	DeveloperID string `json:"developer_id,omitempty" jsonschema:"the Play Console developer account id (the number in the Console URL); omit to use the configured one"`
	Email       string `json:"email,omitempty" jsonschema:"show only the user with this address"`
}

// UserGrant is one user's access to one app.
type UserGrant struct {
	PackageName string   `json:"package_name"`
	Permissions []string `json:"app_level_permissions,omitempty"`
}

// PlayUser is one member of the developer account.
type PlayUser struct {
	Email string `json:"email"`
	// AccessState is the API's own enum: INVITED, INVITATION_EXPIRED,
	// ACCESS_GRANTED, ACCESS_EXPIRED. An invitation that was never accepted
	// looks exactly like a member until you read this.
	AccessState string `json:"access_state,omitempty"`
	// ExpirationTime is when this user's access lapses, empty for permanent
	// access.
	ExpirationTime string `json:"expiration_time,omitempty"`
	// Partial marks a user the API could only describe in part, because they
	// hold permissions on apps this credential cannot see. Their grant list is
	// therefore not the whole story.
	Partial bool `json:"partial,omitempty"`
	// Permissions are the account-wide permission enums (the ones ending in
	// _GLOBAL), which apply to every app rather than to a named one.
	Permissions []string    `json:"developer_permissions,omitempty"`
	Grants      []UserGrant `json:"grants,omitempty"`
}

// UsersResult lists the account's users.
type UsersResult struct {
	DeveloperID string     `json:"developer_id"`
	Users       []PlayUser `json:"users"`
	// Truncated says the listing stopped at the page cap, so an absent user is
	// not evidence they are not on the account.
	Truncated bool `json:"truncated,omitempty"`
}

func (r UsersResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.Users))
	for _, u := range r.Users {
		packages := make([]string, 0, len(u.Grants))
		for _, g := range u.Grants {
			packages = append(packages, g.PackageName)
		}
		rows = append(rows, jsonRow(map[string]string{
			"email":        u.Email,
			"access_state": u.AccessState,
			"expires":      u.ExpirationTime,
			"apps":         strings.Join(packages, " "),
		}))
	}
	return rows, []string{"email", "access_state", "expires", "apps"}
}

// apiGrant is the wire shape of a Grant.
type apiGrant struct {
	Name                string   `json:"name"`
	PackageName         string   `json:"packageName"`
	AppLevelPermissions []string `json:"appLevelPermissions"`
}

// pkg is the app a grant is for. The API names the resource
// developers/{d}/users/{email}/grants/{packageName} and does not always repeat
// the package in its own field, so the name is the fallback.
func (g apiGrant) pkg() string {
	if g.PackageName != "" {
		return g.PackageName
	}
	if i := strings.LastIndex(g.Name, "/"); i >= 0 {
		return g.Name[i+1:]
	}
	return ""
}

// apiUser is the wire shape of a User.
type apiUser struct {
	Name                        string     `json:"name"`
	Email                       string     `json:"email"`
	AccessState                 string     `json:"accessState"`
	ExpirationTime              string     `json:"expirationTime"`
	Partial                     bool       `json:"partial"`
	DeveloperAccountPermissions []string   `json:"developerAccountPermissions"`
	Grants                      []apiGrant `json:"grants"`
}

func (u apiUser) toPlayUser() PlayUser {
	out := PlayUser{
		Email:          u.Email,
		AccessState:    u.AccessState,
		ExpirationTime: u.ExpirationTime,
		Partial:        u.Partial,
		Permissions:    u.DeveloperAccountPermissions,
	}
	for _, g := range u.Grants {
		out.Grants = append(out.Grants, UserGrant{PackageName: g.pkg(), Permissions: g.AppLevelPermissions})
	}
	return out
}

// A user and a grant are addressed two ways, and they are not interchangeable.
//
// The URL needs its segments escaped, because both an email and a package name
// arrive from the caller and a stray slash would silently retarget the request.
// The `name` field inside the request body is the resource name itself, which
// the API compares against the decoded path — so it is spelled raw. Both User
// and Grant mark it Required, and a body without it is rejected even though
// every identifying part is already in the URL.

// usersPath is the collection every users call hangs off.
func usersPath(developerID string) string {
	return "developers/" + developerID + "/users"
}

// userPath addresses one user in a URL.
func userPath(developerID, email string) string {
	return usersPath(developerID) + "/" + url.PathEscape(email)
}

// grantPath addresses one user's access to one app in a URL.
func grantPath(developerID, email, packageName string) string {
	return userPath(developerID, email) + "/grants/" + url.PathEscape(packageName)
}

// userResourceName is the `name` a User body must carry.
func userResourceName(developerID, email string) string {
	return usersPath(developerID) + "/" + email
}

// grantResourceName is the `name` a Grant body must carry.
func grantResourceName(developerID, email, packageName string) string {
	return userResourceName(developerID, email) + "/grants/" + packageName
}

// listUsers walks the account's users. Shared by the read tool and by every
// write that has to know what a user holds before it changes it.
func listUsers(ctx context.Context, c *Client, developerID string) ([]apiUser, bool, error) {
	var users []apiUser
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			pagedResponse
			Users []apiUser `json:"users"`
		}
		if err := c.do(ctx, http.MethodGet, usersPath(developerID), query, nil, &page); err != nil {
			return "", false, err
		}
		users = append(users, page.Users...)
		return page.next(), true, nil
	})
	if err != nil {
		return nil, false, err
	}
	return users, truncated, nil
}

// findUser locates one user by address, case-insensitively — Play echoes back
// whatever case the invitation was sent in, and nobody remembers it.
//
// A missing user is an error rather than an empty result: every caller is about
// to change that user's access, and "granted nothing to nobody" is not an
// outcome worth previewing.
func findUser(ctx context.Context, c *Client, developerID, email string) (apiUser, error) {
	users, _, err := listUsers(ctx, c, developerID)
	if err != nil {
		return apiUser{}, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return apiUser{}, fmt.Errorf("no user %s on developer account %s — invite them first with `rollout play user invite --email %s` (see `rollout play users`)", email, developerID, email)
}

// grantFor returns the user's existing access to one app, and whether they have
// any. It decides create-versus-update: the API has no upsert, and posting a
// grant that already exists fails.
func (u apiUser) grantFor(packageName string) (apiGrant, bool) {
	for _, g := range u.Grants {
		if strings.EqualFold(g.pkg(), packageName) {
			return g, true
		}
	}
	return apiGrant{}, false
}

// runUsers lists the developer account's users and what each of them may do.
func runUsers(ctx context.Context, c *Client, args UsersArgs) (UsersResult, error) {
	developerID, err := c.resolveDeveloperID(args.DeveloperID)
	if err != nil {
		return UsersResult{}, err
	}
	users, truncated, err := listUsers(ctx, c, developerID)
	if err != nil {
		return UsersResult{}, toolError("users", err)
	}

	out := UsersResult{DeveloperID: developerID, Truncated: truncated}
	wanted := strings.TrimSpace(args.Email)
	for _, u := range users {
		if wanted != "" && !strings.EqualFold(u.Email, wanted) {
			continue
		}
		out.Users = append(out.Users, u.toPlayUser())
	}
	return out, nil
}

// --- permission enums ---

// developerLevelPermissions are the account-wide permissions, which apply to
// every app on the account rather than to one named app. Play spells them with
// a _GLOBAL suffix, which is the only thing distinguishing several of them from
// their app-level twins.
var developerLevelPermissions = []string{
	"CAN_SEE_ALL_APPS",
	"CAN_VIEW_FINANCIAL_DATA_GLOBAL",
	"CAN_MANAGE_PERMISSIONS_GLOBAL",
	"CAN_EDIT_GAMES_GLOBAL",
	"CAN_PUBLISH_GAMES_GLOBAL",
	"CAN_REPLY_TO_REVIEWS_GLOBAL",
	"CAN_MANAGE_PUBLIC_APKS_GLOBAL",
	"CAN_MANAGE_TRACK_APKS_GLOBAL",
	"CAN_MANAGE_TRACK_USERS_GLOBAL",
	"CAN_MANAGE_PUBLIC_LISTING_GLOBAL",
	"CAN_MANAGE_DRAFT_APPS_GLOBAL",
	"CAN_CREATE_MANAGED_PLAY_APPS_GLOBAL",
	"CAN_CHANGE_MANAGED_PLAY_SETTING_GLOBAL",
	"CAN_MANAGE_ORDERS_GLOBAL",
	"CAN_MANAGE_APP_CONTENT_GLOBAL",
	"CAN_VIEW_NON_FINANCIAL_DATA_GLOBAL",
	"CAN_VIEW_APP_QUALITY_GLOBAL",
	"CAN_MANAGE_DEEPLINKS_GLOBAL",
	"CAN_VIEW_CONNECTED_APPS_GLOBAL",
	"CAN_EDIT_CONNECTED_APPS_GLOBAL",
}

// appLevelPermissions are the permissions a grant carries for one app.
var appLevelPermissions = []string{
	"CAN_ACCESS_APP",
	"CAN_VIEW_FINANCIAL_DATA",
	"CAN_MANAGE_PERMISSIONS",
	"CAN_REPLY_TO_REVIEWS",
	"CAN_MANAGE_PUBLIC_APKS",
	"CAN_MANAGE_TRACK_APKS",
	"CAN_MANAGE_TRACK_USERS",
	"CAN_MANAGE_PUBLIC_LISTING",
	"CAN_MANAGE_DRAFT_APPS",
	"CAN_MANAGE_ORDERS",
	"CAN_MANAGE_APP_CONTENT",
	"CAN_VIEW_NON_FINANCIAL_DATA",
	"CAN_VIEW_APP_QUALITY",
	"CAN_MANAGE_DEEPLINKS",
}

// permissionSet is one of Play's two permission vocabularies, with what an
// error needs to say when a value from the other one turns up in it.
type permissionSet struct {
	// kind names this vocabulary the way the error message reads it.
	kind string
	// elsewhere tells the caller where a value from this set does belong,
	// spelled as commands rather than as a flag: the flag that takes an
	// app-level permission is called --permissions on `user grant` and
	// --app-permissions on `user invite`, so naming one flag would be wrong
	// half the time.
	elsewhere string
	allowed   []string
}

var (
	developerPermissionSet = permissionSet{
		kind:      "an account-level",
		elsewhere: "account-level permissions are set when inviting a user (`rollout play user invite --permissions`), and apply to every app",
		allowed:   developerLevelPermissions,
	}
	appPermissionSet = permissionSet{
		kind:      "an app-level",
		elsewhere: "app-level permissions belong to one app — pass them with `rollout play user invite --app-permissions` or `rollout play user grant --permissions`",
		allowed:   appLevelPermissions,
	}
)

// normalize validates a permission list against this vocabulary, upper-casing
// and de-duplicating on the way.
//
// The API rejects an unknown enum with a message that quotes neither the value
// nor the alternatives, and the two vocabularies differ only by a _GLOBAL
// suffix — so a value from the wrong one is named as such, with where it does
// belong.
func (s permissionSet) normalize(values []string, other permissionSet) ([]string, error) {
	valid := map[string]bool{}
	for _, p := range s.allowed {
		valid[p] = true
	}
	elsewhere := map[string]bool{}
	for _, p := range other.allowed {
		elsewhere[p] = true
	}

	seen := map[string]bool{}
	var out []string
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			part = strings.ToUpper(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if !valid[part] {
				if elsewhere[part] {
					return nil, fmt.Errorf("%s is %s permission, not %s one — %s", part, other.kind, s.kind, other.elsewhere)
				}
				return nil, fmt.Errorf("unknown %s permission %q — expected one of: %s", s.kind, part, strings.Join(s.allowed, ", "))
			}
			if seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	sort.Strings(out)
	return out, nil
}

// normalizeDeveloperPermissions validates account-wide permissions.
func normalizeDeveloperPermissions(values []string) ([]string, error) {
	return developerPermissionSet.normalize(values, appPermissionSet)
}

// normalizeAppPermissions validates the permissions a grant carries.
func normalizeAppPermissions(values []string) ([]string, error) {
	return appPermissionSet.normalize(values, developerPermissionSet)
}

// --- CLI front-end ---

var (
	usersArgs   UsersArgs
	usersFormat string
)

// usersCmd lists the account; userCmd (in tool_users_write.go) parents the
// writes, mirroring the reviews/review split.
var usersCmd = &cobra.Command{
	Use:         "users",
	Short:       "List the users on the developer account and what they may do",
	Annotations: mcpTool("users"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, usersArgs, usersFormat, runUsers)
	},
}

// addDeveloperIDFlag registers the --developer-id flag the users tools accept.
func addDeveloperIDFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "developer-id", "", "Play Console developer account id (falls back to the configured one)")
}

func init() {
	addDeveloperIDFlag(usersCmd, &usersArgs.DeveloperID)
	usersCmd.Flags().StringVar(&usersArgs.Email, "email", "", "show only this user")
	addFormatFlag(usersCmd, &usersFormat)
}
