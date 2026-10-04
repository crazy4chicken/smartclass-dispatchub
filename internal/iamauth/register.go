package iamauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// registeredBy identifies this service in the teamusers permission registry.
const registeredBy = "dispatchub"

// envTeamusersURL names the configuration variable reported when the base URL
// is missing; it is kept local so this package does not depend on config.
const envTeamusersURL = "DISPATCH_TEAMUSERS_URL"

// PermissionKeys is the complete dispatchub permission catalog. Every route
// requires exactly one of these keys; the any → team → own ladder is applied by
// Authorizer.Require.
var PermissionKeys = []string{
	"dispatch:read:any",
	"dispatch:read:team",
	"dispatch:read:own",
	"dispatch:manage:any",
	"dispatch:manage:team",
	"dispatch:manage:own",
	"dispatch:control:any",
	"dispatch:control:team",
	"dispatch:control:own",
}

// permissionDescriptions is the human-readable catalog registered in
// teamusers. Keys without an entry get a generic description.
var permissionDescriptions = map[string]string{
	"dispatch:read:any":     "Read every dispatch resource: terms, period tables, room bindings, timetable imports, recording sessions and live room state.",
	"dispatch:read:team":    "Read dispatch resources belonging to the caller's team.",
	"dispatch:read:own":     "Read dispatch resources owned by the caller.",
	"dispatch:manage:any":   "Administer terms, period tables, room bindings and timetable imports for every team.",
	"dispatch:manage:team":  "Administer terms, period tables, room bindings and timetable imports for the caller's team.",
	"dispatch:manage:own":   "Administer room bindings owned by the caller.",
	"dispatch:control:any":  "Start and stop recordings, switch cameras and capture photos in every room.",
	"dispatch:control:team": "Start and stop recordings, switch cameras and capture photos in the caller's team's rooms.",
	"dispatch:control:own":  "Start and stop recordings, switch cameras and capture photos in rooms owned by the caller.",
}

// RegisterPermissions upserts every key through POST {base}/permissions/ with
// the supplied admin bearer token. teamusers treats the endpoint as an upsert
// keyed by the permission string, so re-running is safe. An empty keys slice
// registers PermissionKeys.
//
// Registration is best-effort across the catalog: every key is attempted, and
// a partial failure returns an error naming how many keys failed while the
// successful ones stay registered.
func (a *Authorizer) RegisterPermissions(ctx context.Context, adminToken string, keys []string) error {
	if a == nil || strings.TrimSpace(a.baseURL) == "" {
		return errors.New("iam: " + envTeamusersURL + " is required to register permissions")
	}
	token := strings.TrimSpace(adminToken)
	if token == "" {
		return errors.New("iam: an admin bearer token is required to register permissions")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(keys) == 0 {
		keys = PermissionKeys
	}
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	attempted := 0
	var failures []error
	for _, rawKey := range keys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		attempted++
		if err := registerPermission(ctx, client, a.baseURL, token, key); err != nil {
			failures = append(failures, err)
			a.log.Error("register permission failed", "key", key, "error", err)
			continue
		}
		a.log.Debug("permission registered", "key", key)
	}
	if len(failures) > 0 {
		return fmt.Errorf("iam: register permissions: %d of %d keys failed: %w", len(failures), attempted, errors.Join(failures...))
	}
	a.log.Info("permission catalog registered", "count", attempted)
	return nil
}

// registerPermission performs one catalog upsert.
func registerPermission(ctx context.Context, client *http.Client, base, token, key string) error {
	description := permissionDescriptions[key]
	if description == "" {
		description = "Dispatch Hub permission registered by dispatchub."
	}
	body, err := json.Marshal(struct {
		Key          string `json:"key"`
		Description  string `json:"description"`
		RegisteredBy string `json:"registered_by"`
	}{Key: key, Description: description, RegisteredBy: registeredBy})
	if err != nil {
		return fmt.Errorf("register %s: encode request: %w", key, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/permissions/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("register %s: create request: %w", key, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("register %s: %w", key, err)
	}
	defer response.Body.Close()
	// The registry answers 201 for an upsert and 200 when the row is unchanged.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("register %s: HTTP %d: %s", key, response.StatusCode, responseDetail(response.Body))
	}
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxTokenResponseBytes))
	return nil
}
