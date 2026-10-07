package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/config"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/iamauth"
)

// blankEnv clears the configuration variables a subcommand test must control,
// so the test sees exactly what it sets itself.
func blankEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

// TestRegisterPermissionsNeedsOnlyTeamusers pins the runtime dependencies of
// the subcommand: it talks to teamusers with an admin token, so a host that
// only registers the catalog is not asked for the database, the webcam-server
// endpoint or the service client credentials.
func TestRegisterPermissionsNeedsOnlyTeamusers(t *testing.T) {
	blankEnv(t,
		config.EnvFile, config.EnvDSN, config.EnvDev, config.EnvWebcamURL,
		config.EnvTeamusersClientID, config.EnvTeamusersClientSecret,
	)

	var calls atomic.Int32
	teamusers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/permissions/" {
			t.Errorf("request = %s %s, want POST /permissions/", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Errorf("Authorization = %q, want the admin bearer token", got)
		}
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer teamusers.Close()
	t.Setenv(config.EnvTeamusersURL, teamusers.URL)

	if code := run([]string{"register-permissions", "-token", "admin-token"}); code != 0 {
		t.Fatalf("run(register-permissions) = %d, want 0", code)
	}
	if got, want := calls.Load(), int32(len(iamauth.PermissionKeys)); got != want {
		t.Fatalf("teamusers calls = %d, want the %d catalog keys", got, want)
	}
}

// TestRegisterPermissionsRequiresTeamusers covers the other side of the same
// contract: the one endpoint the subcommand does use stays mandatory.
func TestRegisterPermissionsRequiresTeamusers(t *testing.T) {
	blankEnv(t,
		config.EnvFile, config.EnvDSN, config.EnvDev, config.EnvWebcamURL,
		config.EnvTeamusersClientID, config.EnvTeamusersClientSecret,
		config.EnvTeamusersURL,
	)

	if code := run([]string{"register-permissions", "-token", "admin-token"}); code != 2 {
		t.Fatalf("run(register-permissions) = %d, want 2 without %s", code, config.EnvTeamusersURL)
	}
}
