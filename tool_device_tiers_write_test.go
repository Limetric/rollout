package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempTextFile writes a file a tool that reads one can be pointed at.
func writeTempTextFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const deviceTierConfigJSON = `{
	"deviceGroups": [
		{"name": "high", "deviceSelectors": [{"deviceRam": {"minBytes": "6000000000"}}]},
		{"name": "low", "deviceSelectors": [{"deviceRam": {"maxBytes": "2000000000"}}]}
	],
	"deviceTierSet": {"deviceTiers": [
		{"level": 2, "deviceGroupNames": ["high"]},
		{"level": 1, "deviceGroupNames": ["low"]}
	]},
	"userCountrySets": [{"name": "latam", "countryCodes": ["BR", "MX"]}]
}`

func TestCreateDeviceTierConfigPostsTheWholeFile(t *testing.T) {
	var posted recordedRequest
	var body string
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deviceTierConfigs") {
			posted = recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
			raw, _ := io.ReadAll(r.Body)
			body = string(raw)
		}
		writeJSON(w, http.StatusOK, `{"deviceTierConfigId":"77"}`)
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempTextFile(t, "tiers.json", deviceTierConfigJSON)
	preview, err := runCreateDeviceTierConfig(ctx, client, CreateDeviceTierConfigArgs{File: path})
	if err != nil {
		t.Fatalf("runCreateDeviceTierConfig: %v", err)
	}
	if preview.Applied || preview.ConfirmToken == "" {
		t.Fatalf("first call must not apply: %+v", preview)
	}
	// A tier config is read back by its numbers; the group names are what a
	// person recognizes in a preview.
	for _, want := range []string{"2 device groups", "high, low", "2 tiers", "1 country set"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview does not describe %q:\n%s", want, preview.Preview)
		}
	}

	applyPreview(t, ctx, client, "create_device_tier_config", preview.ConfirmToken)

	if posted.Path != "/androidpublisher/v3/applications/com.example.app/deviceTierConfigs" {
		t.Errorf("posted to %q", posted.Path)
	}
	// Off by default, exactly as the API has it.
	if posted.Query != "" {
		t.Errorf("query = %q, want allowUnknownDevices left off", posted.Query)
	}
	// The document is staged whole, so the confirm sends what was previewed
	// even from another process.
	if !strings.Contains(body, "deviceTierSet") || !strings.Contains(body, "latam") {
		t.Errorf("the config did not reach the API intact: %s", body)
	}
}

func TestCreateDeviceTierConfigPassesAllowUnknownDevices(t *testing.T) {
	var query string
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deviceTierConfigs") {
			query = r.URL.RawQuery
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runCreateDeviceTierConfig(ctx, client, CreateDeviceTierConfigArgs{
		File: writeTempTextFile(t, "tiers.json", deviceTierConfigJSON), AllowUnknownDevices: true,
	})
	if err != nil {
		t.Fatalf("runCreateDeviceTierConfig: %v", err)
	}
	if !strings.Contains(preview.Preview, "Unknown devices are allowed") {
		t.Errorf("the preview should say the catalogue check is off:\n%s", preview.Preview)
	}
	applyPreview(t, ctx, client, "create_device_tier_config", preview.ConfirmToken)

	// The parameter belongs to the staged intent: leaving it out at apply time
	// would change what the call does.
	if query != "allowUnknownDevices=true" {
		t.Errorf("query = %q", query)
	}
}

// TestCreateDeviceTierConfigRefusesABadFile: the API's own rejection quotes a
// field path against a document it does not echo back.
func TestCreateDeviceTierConfigRefusesABadFile(t *testing.T) {
	client := newTestClient(t, newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}))
	ctx := context.Background()

	_, err := runCreateDeviceTierConfig(ctx, client, CreateDeviceTierConfigArgs{
		File: writeTempTextFile(t, "tiers.json", `not json at all`),
	})
	if err == nil || !strings.Contains(err.Error(), "DeviceTierConfig") {
		t.Errorf("expected unparseable JSON to be refused by name: %v", err)
	}

	_, err = runCreateDeviceTierConfig(ctx, client, CreateDeviceTierConfigArgs{
		File: writeTempTextFile(t, "empty.json", `{"deviceGroups": []}`),
	})
	if err == nil || !strings.Contains(err.Error(), "empty configuration") {
		t.Errorf("expected an empty config to be refused: %v", err)
	}

	_, err = runCreateDeviceTierConfig(ctx, client, CreateDeviceTierConfigArgs{File: "/no/such/file.json"})
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("expected a missing file to be reported: %v", err)
	}
}
