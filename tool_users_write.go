package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Writes against the developer account's users and their per-app grants.
//
// None of these is edit-scoped: users live outside the publishing transaction,
// so they go through dispatchDirect. That is also why an invite that fails
// half-way is reported as half-applied rather than rolled back — there is no
// transaction to roll back to, and pretending otherwise would send someone to
// re-invite a user who already exists.

// InviteUserArgs invites someone to the developer account.
type InviteUserArgs struct {
	DeveloperID    string   `json:"developer_id,omitempty" jsonschema:"the Play Console developer account id; omit to use the configured one"`
	Email          string   `json:"email" jsonschema:"the Google account address to invite"`
	Permissions    []string `json:"permissions,omitempty" jsonschema:"account-level permission enums, which apply to every app (e.g. CAN_VIEW_APP_QUALITY_GLOBAL)"`
	Apps           []string `json:"apps,omitempty" jsonschema:"package names to grant access to; each gets the app_permissions below"`
	AppPermissions []string `json:"app_permissions,omitempty" jsonschema:"per-app permission enums for every package in apps (e.g. CAN_MANAGE_TRACK_APKS)"`
	Expires        string   `json:"expires,omitempty" jsonschema:"RFC 3339 timestamp when the access should lapse; omit for permanent access"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runInviteUser stages or applies an invitation.
//
// Play models this as two resources: the user, and one grant per app. The
// preview spells out every permission enum it would send, because these are the
// arguments nobody can check by reading them back — the difference between
// CAN_MANAGE_TRACK_APKS and CAN_MANAGE_PUBLIC_APKS is who can ship to
// production.
func runInviteUser(ctx context.Context, c *Client, args InviteUserArgs) (WriteResult, error) {
	const tool = "invite_user"
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	developerID, err := c.resolveDeveloperID(args.DeveloperID)
	if err != nil {
		return WriteResult{}, err
	}
	email, err := normalizeUserEmail(args.Email)
	if err != nil {
		return WriteResult{}, err
	}
	accountPermissions, err := normalizeDeveloperPermissions(args.Permissions)
	if err != nil {
		return WriteResult{}, err
	}
	apps, appPermissions, err := normalizeGrantArgs(args.Apps, args.AppPermissions)
	if err != nil {
		return WriteResult{}, err
	}
	if len(accountPermissions) == 0 && len(apps) == 0 {
		return WriteResult{}, fmt.Errorf("an invitation with no permissions grants nothing — pass --permissions for account-wide access, or --apps with --app-permissions for one app")
	}
	expires, err := parseExpiry(args.Expires)
	if err != nil {
		return WriteResult{}, err
	}

	users, _, err := listUsers(ctx, c, developerID)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return WriteResult{}, fmt.Errorf("%s is already on developer account %s (%s) — change their access with `rollout play user grant` instead", email, developerID, orNone(u.AccessState))
		}
	}

	user := map[string]any{
		"name":  userResourceName(developerID, email),
		"email": email,
	}
	if len(accountPermissions) > 0 {
		user["developerAccountPermissions"] = accountPermissions
	}
	if expires != "" {
		user["expirationTime"] = expires
	}
	body, err := json.Marshal(user)
	if err != nil {
		return WriteResult{}, err
	}

	requests := []editRequest{{
		Method: http.MethodPost, Path: usersPath(developerID), Body: body,
		Describe: "invite " + email,
	}}
	for _, pkg := range apps {
		grant, err := json.Marshal(map[string]any{
			"name":                grantResourceName(developerID, email, pkg),
			"packageName":         pkg,
			"appLevelPermissions": appPermissions,
		})
		if err != nil {
			return WriteResult{}, err
		}
		requests = append(requests, editRequest{
			Method: http.MethodPost, Path: userPath(developerID, email) + "/grants", Body: grant,
			Describe: "grant on " + pkg,
		})
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Invite %s to developer account %s", email, developerID)
	if len(accountPermissions) > 0 {
		fmt.Fprintf(&summary, " with account-level %s", strings.Join(accountPermissions, ", "))
	} else {
		summary.WriteString(" with no account-level permissions")
	}
	if len(apps) > 0 {
		fmt.Fprintf(&summary, "; on %s: %s", strings.Join(apps, ", "), strings.Join(appPermissions, ", "))
	}
	if expires != "" {
		fmt.Fprintf(&summary, "; access expires %s", expires)
	}
	if len(apps) > 0 {
		summary.WriteString("\nThe invitation and each grant are separate API calls — Play has no transaction across them, so a failure part-way leaves the earlier ones applied.")
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: developerAccount(developerID), Dispatch: dispatchDirect,
		Summary: summary.String(),
		Payload: editPayload{Requests: requests},
	})
}

// SetGrantArgs sets what one user may do with one app.
type SetGrantArgs struct {
	DeveloperID string   `json:"developer_id,omitempty" jsonschema:"the Play Console developer account id; omit to use the configured one"`
	Email       string   `json:"email" jsonschema:"the user whose access to change; they must already be on the account"`
	PackageName string   `json:"package_name,omitempty" jsonschema:"the app to grant access to; omit to use the configured default app"`
	Permissions []string `json:"permissions" jsonschema:"the app-level permission enums this user should have; this replaces their current ones"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSetGrant stages or applies a change to one user's access to one app.
//
// The API has no upsert: creating a grant that exists fails, and patching one
// that does not fails too. So the current grant is read first — which is also
// what lets the preview diff a full replacement instead of presenting it as an
// addition.
func runSetGrant(ctx context.Context, c *Client, args SetGrantArgs) (WriteResult, error) {
	const tool = "set_grant"
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	developerID, email, pkg, user, err := resolveGrantTarget(ctx, c, args.DeveloperID, args.Email, args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	permissions, err := normalizeAppPermissions(args.Permissions)
	if err != nil {
		return WriteResult{}, err
	}
	if len(permissions) == 0 {
		return WriteResult{}, fmt.Errorf("no permissions given — pass --permissions CAN_MANAGE_TRACK_APKS (to take all access away, use `rollout play user revoke`)")
	}

	body, err := json.Marshal(map[string]any{
		"name":                grantResourceName(developerID, email, pkg),
		"packageName":         pkg,
		"appLevelPermissions": permissions,
	})
	if err != nil {
		return WriteResult{}, err
	}
	request := editRequest{
		Method: http.MethodPost, Path: userPath(developerID, email) + "/grants", Body: body,
		Describe: "grant on " + pkg,
	}
	existing, held := user.grantFor(pkg)
	if held {
		// updateMask is what keeps a patch from clearing fields it did not
		// mention; without it the API treats absent fields as cleared.
		request = editRequest{
			Method: http.MethodPatch, Path: grantPath(developerID, email, pkg), Body: body,
			Query:    map[string]string{"updateMask": "appLevelPermissions"},
			Describe: "grant on " + pkg,
		}
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect,
		Summary: fmt.Sprintf("Set what %s may do with %s: %s",
			email, pkg, describePermissionChange(existing.AppLevelPermissions, permissions)),
		Payload: editPayload{Requests: []editRequest{request}},
	})
}

// RevokeGrantArgs takes one user's access to one app away.
type RevokeGrantArgs struct {
	DeveloperID string `json:"developer_id,omitempty" jsonschema:"the Play Console developer account id; omit to use the configured one"`
	Email       string `json:"email" jsonschema:"the user whose access to revoke"`
	PackageName string `json:"package_name,omitempty" jsonschema:"the app to revoke access to; omit to use the configured default app"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runRevokeGrant stages or applies the removal of one user's access to one app.
//
// Two confirmations: the API will not tell you afterwards what the grant held,
// so a revoke performed on the wrong person cannot be put back from anything
// rollout can read. The preview names the permissions being taken away for
// exactly that reason.
func runRevokeGrant(ctx context.Context, c *Client, args RevokeGrantArgs) (WriteResult, error) {
	const tool = "revoke_grant"
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	developerID, email, pkg, user, err := resolveGrantTarget(ctx, c, args.DeveloperID, args.Email, args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	existing, held := user.grantFor(pkg)
	if !held {
		return WriteResult{}, fmt.Errorf("%s holds no grant on %s — there is nothing to revoke (see `rollout play users --email %s`)", email, pkg, email)
	}

	summary := fmt.Sprintf("Revoke %s's access to %s, removing %s",
		email, pkg, strings.Join(existing.AppLevelPermissions, ", "))
	if len(existing.AppLevelPermissions) == 0 {
		summary = fmt.Sprintf("Revoke %s's access to %s", email, pkg)
	}
	if len(user.DeveloperAccountPermissions) > 0 {
		summary += fmt.Sprintf("\nThey keep their account-level permissions (%s), which apply to every app.",
			strings.Join(user.DeveloperAccountPermissions, ", "))
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect,
		Summary: summary, RequiresDouble: true,
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodDelete, Path: grantPath(developerID, email, pkg),
			Describe: "grant on " + pkg,
		}}},
	})
}

// RemoveUserArgs removes someone from the developer account entirely.
type RemoveUserArgs struct {
	DeveloperID string `json:"developer_id,omitempty" jsonschema:"the Play Console developer account id; omit to use the configured one"`
	Email       string `json:"email" jsonschema:"the user to remove from the developer account"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runRemoveUser stages or applies the removal of a user from the account.
//
// Two confirmations: this drops every grant the user held at once, and nothing
// in the API can list what they were afterwards. The preview is therefore the
// only record — it names each app and each permission.
func runRemoveUser(ctx context.Context, c *Client, args RemoveUserArgs) (WriteResult, error) {
	const tool = "remove_user"
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	developerID, err := c.resolveDeveloperID(args.DeveloperID)
	if err != nil {
		return WriteResult{}, err
	}
	email, err := normalizeUserEmail(args.Email)
	if err != nil {
		return WriteResult{}, err
	}
	user, err := findUser(ctx, c, developerID, email)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Remove %s from developer account %s", user.Email, developerID)
	if len(user.DeveloperAccountPermissions) > 0 {
		fmt.Fprintf(&summary, "\nAccount-level permissions lost: %s", strings.Join(user.DeveloperAccountPermissions, ", "))
	}
	for _, g := range user.Grants {
		fmt.Fprintf(&summary, "\nAccess lost to %s: %s", g.pkg(), strings.Join(g.AppLevelPermissions, ", "))
	}
	if user.Partial {
		summary.WriteString("\nPlay reports this user as partial: they hold permissions on apps these credentials cannot see, so the list above is incomplete.")
	}
	summary.WriteString("\nThe API cannot list a removed user's permissions afterwards — this preview is the only record of what they held.")

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: developerAccount(developerID), Dispatch: dispatchDirect,
		Summary: summary.String(), RequiresDouble: true,
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodDelete, Path: userPath(developerID, user.Email),
			Describe: "remove " + user.Email,
		}}},
	})
}

// --- shared helpers ---

// developerAccount is how a developer-account-scoped write names its subject in
// the staged record, where a package name would otherwise go. It is what the
// preview header and the audit line show.
func developerAccount(developerID string) string { return "developers/" + developerID }

// resolveGrantTarget resolves the account, the user, and the app a grant write
// acts on, and reads the user back so the caller can see what they already
// hold.
func resolveGrantTarget(ctx context.Context, c *Client, developerIDArg, emailArg, packageArg string) (developerID, email, pkg string, user apiUser, err error) {
	developerID, err = c.resolveDeveloperID(developerIDArg)
	if err != nil {
		return "", "", "", apiUser{}, err
	}
	email, err = normalizeUserEmail(emailArg)
	if err != nil {
		return "", "", "", apiUser{}, err
	}
	pkg, err = c.resolvePackage(packageArg)
	if err != nil {
		return "", "", "", apiUser{}, err
	}
	user, err = findUser(ctx, c, developerID, email)
	if err != nil {
		return "", "", "", apiUser{}, err
	}
	// Play echoes the address in whatever case the invitation used, and the
	// email is a path segment: addressing the grant with the caller's spelling
	// would 404 on an account where they differ.
	return developerID, user.Email, pkg, user, nil
}

// normalizeUserEmail checks the one thing worth checking locally. Play accepts
// only Google accounts, which is not something this can verify, but an address
// with no @ is a typo every time.
func normalizeUserEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", fmt.Errorf("email is required — pass --email person@example.com")
	}
	if !strings.Contains(email, "@") {
		return "", fmt.Errorf("%q is not an email address — Play users are named by their Google account address", email)
	}
	return email, nil
}

// normalizeGrantArgs validates the app list and its permissions together: one
// without the other is always a mistake, and the API would accept "grant access
// to three apps, permitting nothing" without comment.
func normalizeGrantArgs(apps, permissions []string) ([]string, []string, error) {
	var packages []string
	seen := map[string]bool{}
	for _, raw := range apps {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" && !seen[part] {
				seen[part] = true
				packages = append(packages, part)
			}
		}
	}
	appPermissions, err := normalizeAppPermissions(permissions)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case len(packages) == 0 && len(appPermissions) > 0:
		return nil, nil, fmt.Errorf("--app-permissions names permissions but no app — add --apps com.example.app, or use --permissions for account-wide access")
	case len(packages) > 0 && len(appPermissions) == 0:
		return nil, nil, fmt.Errorf("--apps names %s but no permissions — add --app-permissions CAN_MANAGE_TRACK_APKS; a grant with none permits nothing", strings.Join(packages, ", "))
	}
	return packages, appPermissions, nil
}

// parseExpiry accepts the RFC 3339 timestamp Play stores as expirationTime.
func parseExpiry(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("expires %q is not an RFC 3339 timestamp — pass something like 2026-01-31T00:00:00Z", value)
	}
	return t.UTC().Format(time.RFC3339), nil
}

// describePermissionChange renders what a full replacement actually does, so a
// list that silently drops the permission you did not retype is visible before
// it is applied rather than after.
func describePermissionChange(before, after []string) string {
	added, removed := diffStrings(before, after)
	switch {
	case len(before) == 0:
		return "granting " + strings.Join(after, ", ")
	case len(added) == 0 && len(removed) == 0:
		return "no change (" + strings.Join(after, ", ") + ")"
	case len(removed) == 0:
		return "granting " + strings.Join(added, ", ") + " on top of " + strings.Join(before, ", ")
	case len(added) == 0:
		return "revoking " + strings.Join(removed, ", ") + ", leaving " + strings.Join(after, ", ")
	default:
		return "granting " + strings.Join(added, ", ") + "; revoking " + strings.Join(removed, ", ")
	}
}

// --- CLI front-end ---

var (
	inviteUserArgs  InviteUserArgs
	setGrantArgs    SetGrantArgs
	revokeGrantArgs RevokeGrantArgs
	removeUserArgs  RemoveUserArgs
)

// userCmd groups the writes; `rollout play users` does the reading.
var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage developer account users and their per-app access",
}

var userInviteCmd = &cobra.Command{
	Use:         "invite",
	Short:       "Invite a user to the developer account (previews first; --confirm to apply)",
	Annotations: mcpTool("invite_user"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, inviteUserArgs, runInviteUser)
	},
}

var userGrantCmd = &cobra.Command{
	Use:         "grant",
	Short:       "Set what a user may do with one app (previews first; --confirm to apply)",
	Annotations: mcpTool("set_grant"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, setGrantArgs, runSetGrant)
	},
}

var userRevokeCmd = &cobra.Command{
	Use:         "revoke",
	Short:       "Take away a user's access to one app (two confirmations)",
	Annotations: mcpTool("revoke_grant"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, revokeGrantArgs, runRevokeGrant)
	},
}

var userRemoveCmd = &cobra.Command{
	Use:         "remove",
	Short:       "Remove a user from the developer account (two confirmations)",
	Annotations: mcpTool("remove_user"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, removeUserArgs, runRemoveUser)
	},
}

func init() {
	addDeveloperIDFlag(userInviteCmd, &inviteUserArgs.DeveloperID)
	userInviteCmd.Flags().StringVar(&inviteUserArgs.Email, "email", "", "Google account address to invite (required)")
	userInviteCmd.Flags().StringArrayVar(&inviteUserArgs.Permissions, "permissions", nil, "account-level permissions, e.g. CAN_VIEW_APP_QUALITY_GLOBAL (repeatable or comma-separated)")
	userInviteCmd.Flags().StringArrayVar(&inviteUserArgs.Apps, "apps", nil, "package names to grant access to (repeatable or comma-separated)")
	userInviteCmd.Flags().StringArrayVar(&inviteUserArgs.AppPermissions, "app-permissions", nil, "per-app permissions for every --apps package, e.g. CAN_MANAGE_TRACK_APKS")
	userInviteCmd.Flags().StringVar(&inviteUserArgs.Expires, "expires", "", "RFC 3339 time the access should lapse")
	addConfirmFlag(userInviteCmd, &inviteUserArgs.Confirm)

	addDeveloperIDFlag(userGrantCmd, &setGrantArgs.DeveloperID)
	addPackageFlag(userGrantCmd, &setGrantArgs.PackageName)
	userGrantCmd.Flags().StringVar(&setGrantArgs.Email, "email", "", "the user to change (required)")
	userGrantCmd.Flags().StringArrayVar(&setGrantArgs.Permissions, "permissions", nil, "app-level permissions, e.g. CAN_MANAGE_TRACK_APKS (replaces the current ones)")
	addConfirmFlag(userGrantCmd, &setGrantArgs.Confirm)

	addDeveloperIDFlag(userRevokeCmd, &revokeGrantArgs.DeveloperID)
	addPackageFlag(userRevokeCmd, &revokeGrantArgs.PackageName)
	userRevokeCmd.Flags().StringVar(&revokeGrantArgs.Email, "email", "", "the user to revoke (required)")
	addConfirmFlag(userRevokeCmd, &revokeGrantArgs.Confirm)

	addDeveloperIDFlag(userRemoveCmd, &removeUserArgs.DeveloperID)
	userRemoveCmd.Flags().StringVar(&removeUserArgs.Email, "email", "", "the user to remove (required)")
	addConfirmFlag(userRemoveCmd, &removeUserArgs.Confirm)

	userCmd.AddCommand(userInviteCmd, userGrantCmd, userRevokeCmd, userRemoveCmd)
}
