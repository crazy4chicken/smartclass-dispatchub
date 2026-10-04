package webcam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func staticToken(token string) TokenSource {
	return TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
}

// capturedRequest is one request as the test handler saw it.
type capturedRequest struct {
	method        string
	path          string
	query         string
	authorization string
	accept        string
	contentType   string
	body          string
}

type recorder struct {
	mu    sync.Mutex
	items []capturedRequest
}

func (r *recorder) add(item capturedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, item)
}

func (r *recorder) all() []capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedRequest(nil), r.items...)
}

type testServer struct {
	client *Client
	rec    *recorder
	url    string
}

func newTestServer(t *testing.T, tokens TokenSource, handler http.HandlerFunc) *testServer {
	t.Helper()
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(capturedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			accept:        r.Header.Get("Accept"),
			contentType:   r.Header.Get("Content-Type"),
			body:          string(body),
		})
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return &testServer{
		client: New(server.URL, tokens, 5*time.Second, discardLogger()),
		rec:    rec,
		url:    server.URL,
	}
}

// newAttemptServer is newTestServer plus a 1-based attempt counter and the
// captured requests, for retry and token-refresh assertions.
func newAttemptServer(t *testing.T, tokens TokenSource, handler func(attempt int, w http.ResponseWriter, r *http.Request)) (*Client, *recorder, *int32) {
	t.Helper()
	var count int32
	server := newTestServer(t, tokens, func(w http.ResponseWriter, r *http.Request) {
		handler(int(atomic.AddInt32(&count, 1)), w, r)
	})
	return server.client, server.rec, &count
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func abortHandler() http.HandlerFunc {
	return func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}
}

// TestClientRequestShape pins the upstream route, method, body and headers of
// every typed call (plan §2.1).
func TestClientRequestShape(t *testing.T) {
	ctx := context.Background()
	const responseBody = `{"id":"st1","device_id":"dev1","camera_enum":1,"status":"active",` +
		`"started_at":"2026-03-02T08:00:00Z","command_id":"cmd1","request_id":"req1","items":[],"segments":[]}`

	tests := []struct {
		name   string
		status int
		call   func(*Client) error
		method string
		path   string
		query  string
		body   string
	}{
		{
			name: "device by id", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Device(ctx, "dev1"); return err },
			method: http.MethodGet, path: "/api/devices/dev1/",
		},
		{
			name: "start recording", status: http.StatusCreated,
			call: func(c *Client) error {
				stream, err := c.Start(ctx, "dev1", 1)
				if err == nil && stream.ID != "st1" {
					return fmt.Errorf("stream id = %q, want st1", stream.ID)
				}
				return err
			},
			method: http.MethodPost, path: "/api/devices/dev1/recording/start", body: `{"camera_enum":1}`,
		},
		{
			name: "stop recording", status: http.StatusOK,
			call: func(c *Client) error {
				stream, err := c.Stop(ctx, "dev1", 1)
				if err == nil && stream.Status != StreamActive {
					return fmt.Errorf("stream status = %q, want %q", stream.Status, StreamActive)
				}
				return err
			},
			method: http.MethodPost, path: "/api/devices/dev1/recording/stop", body: `{"camera_enum":1}`,
		},
		{
			name: "switch camera", status: http.StatusAccepted,
			call: func(c *Client) error {
				ack, err := c.Switch(ctx, "dev1", 2)
				if err == nil && (ack.CommandID != "cmd1" || ack.CameraEnum != 1) {
					return fmt.Errorf("ack = %+v", ack)
				}
				return err
			},
			method: http.MethodPost, path: "/api/devices/dev1/camera/switch", body: `{"camera_enum":2}`,
		},
		{
			name: "take photo", status: http.StatusAccepted,
			call: func(c *Client) error {
				ack, err := c.TakePhoto(ctx, "dev1", 0)
				if err == nil && ack.RequestID != "req1" {
					return fmt.Errorf("request id = %q, want req1", ack.RequestID)
				}
				return err
			},
			method: http.MethodPost, path: "/api/devices/dev1/photo", body: `{"camera_enum":0}`,
		},
		{
			name: "list streams with limit", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Streams(ctx, "dev1", 25); return err },
			method: http.MethodGet, path: "/api/devices/dev1/streams", query: "limit=25",
		},
		{
			name: "list streams without limit", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Streams(ctx, "dev1", 0); return err },
			method: http.MethodGet, path: "/api/devices/dev1/streams",
		},
		{
			name: "stream detail", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.StreamDetail(ctx, "st1"); return err },
			method: http.MethodGet, path: "/api/streams/st1/",
		},
		{
			name: "list photos with limit", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Photos(ctx, "dev1", 10); return err },
			method: http.MethodGet, path: "/api/devices/dev1/photos", query: "limit=10",
		},
		{
			name: "photo by id", status: http.StatusOK,
			call:   func(c *Client) error { _, err := c.Photo(ctx, "p1"); return err },
			method: http.MethodGet, path: "/api/photos/p1/",
		},
		{
			name: "ready probe", status: http.StatusOK,
			call:   func(c *Client) error { return c.Ready(ctx) },
			method: http.MethodGet, path: "/readyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t, staticToken("test-token"), jsonHandler(tt.status, responseBody))
			if err := tt.call(server.client); err != nil {
				t.Fatalf("call error = %v, want nil", err)
			}
			requests := server.rec.all()
			if len(requests) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(requests))
			}
			got := requests[0]
			if got.method != tt.method {
				t.Fatalf("method = %s, want %s", got.method, tt.method)
			}
			if got.path != tt.path {
				t.Fatalf("path = %s, want %s", got.path, tt.path)
			}
			if got.query != tt.query {
				t.Fatalf("query = %q, want %q", got.query, tt.query)
			}
			if got.authorization != "Bearer test-token" {
				t.Fatalf("Authorization = %q, want the Bearer credential", got.authorization)
			}
			if got.accept != "application/json" {
				t.Fatalf("Accept = %q, want application/json", got.accept)
			}
			if tt.body == "" {
				if got.contentType != "" {
					t.Fatalf("Content-Type = %q on a bodyless call, want none", got.contentType)
				}
				if got.body != "" {
					t.Fatalf("body = %q on a bodyless call, want none", got.body)
				}
			} else {
				if got.contentType != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got.contentType)
				}
				if got.body != tt.body {
					t.Fatalf("body = %q, want %q", got.body, tt.body)
				}
			}
		})
	}
}

func TestClientBaseURLTrailingSlashIsNormalized(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, `{}`))
	client := New(server.url+"/", staticToken("t"), 5*time.Second, discardLogger())
	if _, err := client.Device(context.Background(), "dev1"); err != nil {
		t.Fatalf("Device() error = %v, want nil", err)
	}
	if got := server.rec.all()[0].path; got != "/api/devices/dev1/" {
		t.Fatalf("path = %q, want a single slash", got)
	}
}

func TestClientDecodesTypedResponses(t *testing.T) {
	ctx := context.Background()

	t.Run("device", func(t *testing.T) {
		body := `{"id":"dev1","name":"A301","location":"Building A","team_id":"team-1","online":true,` +
			`"cameras":[{"camera_enum":0,"resolution":"1920x1080","fps":30,"supported_codec":["h264","mjpeg"]}]}`
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, body))
		device, err := server.client.Device(ctx, "dev1")
		if err != nil {
			t.Fatalf("Device() error = %v, want nil", err)
		}
		if device.ID != "dev1" || device.Name != "A301" || device.Location != "Building A" || !device.Online {
			t.Fatalf("device = %+v", device)
		}
		if device.TeamID == nil || *device.TeamID != "team-1" {
			t.Fatalf("device.TeamID = %v, want team-1", device.TeamID)
		}
		if len(device.Cameras) != 1 || device.Cameras[0].CameraEnum != 0 ||
			device.Cameras[0].FPS != 30 || len(device.Cameras[0].Codecs) != 2 {
			t.Fatalf("device cameras = %+v", device.Cameras)
		}
	})

	t.Run("stream detail", func(t *testing.T) {
		body := `{"id":"st1","device_id":"dev1","camera_enum":1,"status":"completed",` +
			`"started_at":"2026-03-02T08:00:00Z","ended_at":"2026-03-02T09:35:00Z",` +
			`"segments":[{"id":"seg1","stream_id":"st1","segment_seq":1,"size_bytes":4096,"duration_ms":1500,` +
			`"download_url":"https://files.example.com/seg1","created_at":"2026-03-02T08:01:00Z"}]}`
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, body))
		detail, err := server.client.StreamDetail(ctx, "st1")
		if err != nil {
			t.Fatalf("StreamDetail() error = %v, want nil", err)
		}
		if detail.ID != "st1" || detail.Status != StreamCompleted || detail.EndedAt == nil {
			t.Fatalf("stream = %+v", detail.Stream)
		}
		if len(detail.Segments) != 1 {
			t.Fatalf("segments = %+v, want one", detail.Segments)
		}
		segment := detail.Segments[0]
		if segment.SegmentSeq != 1 || segment.SizeBytes != 4096 || segment.DurationMS == nil ||
			*segment.DurationMS != 1500 || segment.DownloadURL == "" {
			t.Fatalf("segment = %+v", segment)
		}
	})

	t.Run("photo with request id", func(t *testing.T) {
		body := `{"id":"p1","device_id":"dev1","camera_enum":0,"content_type":"image/jpeg","size_bytes":1234,` +
			`"request_id":"req1","taken_at":"2026-03-02T08:30:00Z","download_url":"https://files.example.com/p1"}`
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, body))
		photo, err := server.client.Photo(ctx, "p1")
		if err != nil {
			t.Fatalf("Photo() error = %v, want nil", err)
		}
		if photo.ID != "p1" || photo.RequestID == nil || *photo.RequestID != "req1" ||
			photo.DownloadURL == "" || photo.TakenAt.IsZero() {
			t.Fatalf("photo = %+v", photo)
		}
	})

	t.Run("empty photo list", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, `{"items":[]}`))
		photos, err := server.client.Photos(ctx, "dev1", 0)
		if err != nil || len(photos) != 0 {
			t.Fatalf("Photos() = %+v, err %v; want an empty list", photos, err)
		}
	})
}

// TestUpstreamErrorMapping covers RFC 9457 decoding into *UpstreamError and the
// two inspection helpers.
func TestUpstreamErrorMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("problem document", func(t *testing.T) {
		body := `{"type":"about:blank","title":"Conflict","status":409,"detail":"camera_enum 0 is already streaming on device \"dev1\"","instance":"/api/devices/dev1/recording/start"}`
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusConflict, body))
		_, err := server.client.Start(ctx, "dev1", 0)
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if upstream.Status != http.StatusConflict {
			t.Fatalf("status = %d, want 409", upstream.Status)
		}
		if want := `camera_enum 0 is already streaming on device "dev1"`; upstream.Detail != want {
			t.Fatalf("detail = %q, want %q", upstream.Detail, want)
		}
		if upstream.Body == "" {
			t.Fatalf("body is empty, want a bounded copy for logs")
		}
		if !IsStatus(err, http.StatusConflict) || IsStatus(err, http.StatusNotFound) {
			t.Fatalf("IsStatus did not answer for the decoded problem")
		}
		if got := DetailOf(err); got != upstream.Detail {
			t.Fatalf("DetailOf() = %q, want %q", got, upstream.Detail)
		}
		// Callers wrap control errors; inspection must still work.
		wrapped := fmt.Errorf("start session: %w", err)
		if !IsStatus(wrapped, http.StatusConflict) || DetailOf(wrapped) != upstream.Detail {
			t.Fatalf("IsStatus/DetailOf do not unwrap the caller error")
		}
	})

	t.Run("non problem body falls back to the status text", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusBadGateway, "  gateway exploded  "))
		_, err := server.client.Device(ctx, "dev1")
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if upstream.Detail != http.StatusText(http.StatusBadGateway) {
			t.Fatalf("detail = %q, want %q", upstream.Detail, http.StatusText(http.StatusBadGateway))
		}
		if upstream.Body != "gateway exploded" {
			t.Fatalf("body = %q, want the trimmed payload", upstream.Body)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusInternalServerError, ""))
		_, err := server.client.Device(ctx, "dev1")
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if upstream.Detail != http.StatusText(http.StatusInternalServerError) || upstream.Body != "" {
			t.Fatalf("upstream = %+v", upstream)
		}
	})

	t.Run("body is capped", func(t *testing.T) {
		server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusInternalServerError, strings.Repeat("x", 8192)))
		_, err := server.client.Device(ctx, "dev1")
		var upstream *UpstreamError
		if !errors.As(err, &upstream) {
			t.Fatalf("error = %v, want *UpstreamError", err)
		}
		if len(upstream.Body) != maxErrorBodyBytes+len("…") || !strings.HasSuffix(upstream.Body, "…") {
			t.Fatalf("body length = %d, want the cap plus an ellipsis", len(upstream.Body))
		}
	})

	t.Run("foreign errors answer nothing", func(t *testing.T) {
		if IsStatus(nil, http.StatusConflict) || DetailOf(nil) != "" {
			t.Fatalf("IsStatus/DetailOf answered for a nil error")
		}
		plain := errors.New("connection refused")
		if IsStatus(plain, http.StatusConflict) || DetailOf(plain) != "" {
			t.Fatalf("IsStatus/DetailOf answered for a foreign error")
		}
	})
}

// TestRetryPolicy pins the unknown-safe retry rules from plan §9.
func TestRetryPolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("GET retries transport errors twice", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt < 3 {
				abortHandler()(w, nil)
				return
			}
			jsonHandler(http.StatusOK, `{"id":"dev1"}`)(w, nil)
		})
		if _, err := client.Device(ctx, "dev1"); err != nil {
			t.Fatalf("Device() error = %v, want the third attempt to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
	})

	t.Run("start retries once on a transport error", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt == 1 {
				abortHandler()(w, nil)
				return
			}
			jsonHandler(http.StatusCreated, `{"id":"st1"}`)(w, nil)
		})
		if _, err := client.Start(ctx, "dev1", 0); err != nil {
			t.Fatalf("Start() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("stop retries once on a transport error", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt == 1 {
				abortHandler()(w, nil)
				return
			}
			jsonHandler(http.StatusOK, `{"status":"completed"}`)(w, nil)
		})
		if _, err := client.Stop(ctx, "dev1", 0); err != nil {
			t.Fatalf("Stop() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("start retries once on 502", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt == 1 {
				jsonHandler(http.StatusBadGateway, `{"detail":"device connection is unavailable"}`)(w, nil)
				return
			}
			jsonHandler(http.StatusCreated, `{"id":"st1"}`)(w, nil)
		})
		if _, err := client.Start(ctx, "dev1", 0); err != nil {
			t.Fatalf("Start() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("stop retries once on 502", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			if attempt == 1 {
				jsonHandler(http.StatusBadGateway, `{"detail":"device connection is unavailable"}`)(w, nil)
				return
			}
			jsonHandler(http.StatusOK, `{"status":"completed"}`)(w, nil)
		})
		if _, err := client.Stop(ctx, "dev1", 0); err != nil {
			t.Fatalf("Stop() error = %v, want the retry to succeed", err)
		}
		if got := atomic.LoadInt32(count); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("start does not retry a 409", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusConflict, `{"detail":"camera_enum 0 is already streaming on device \"dev1\""}`)(w, r)
		})
		_, err := client.Start(ctx, "dev1", 0)
		if !IsStatus(err, http.StatusConflict) {
			t.Fatalf("Start() error = %v, want a 409 UpstreamError", err)
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("GET does not retry a 502", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusBadGateway, `{"detail":"boo"}`)(w, r)
		})
		_, err := client.Device(ctx, "dev1")
		if !IsStatus(err, http.StatusBadGateway) {
			t.Fatalf("Device() error = %v, want a 502 UpstreamError", err)
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("GET does not retry a 500", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusInternalServerError, "")(w, r)
		})
		if _, err := client.Device(ctx, "dev1"); err == nil {
			t.Fatalf("Device() error = nil, want a 500")
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("photo is never retried on a transport error", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			abortHandler()(w, nil)
		})
		if _, err := client.TakePhoto(ctx, "dev1", 0); err == nil {
			t.Fatalf("TakePhoto() error = nil, want the transport failure")
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1 (a duplicate snapshot is an artifact)", got)
		}
	})

	t.Run("photo is never retried on 502", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusBadGateway, `{"detail":"boo"}`)(w, r)
		})
		if _, err := client.TakePhoto(ctx, "dev1", 0); !IsStatus(err, http.StatusBadGateway) {
			t.Fatalf("TakePhoto() error = %v, want a 502", err)
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("switch is never retried on a transport error", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			abortHandler()(w, nil)
		})
		if _, err := client.Switch(ctx, "dev1", 1); err == nil {
			t.Fatalf("Switch() error = nil, want the transport failure")
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})

	t.Run("ready never retries", func(t *testing.T) {
		client, _, count := newAttemptServer(t, staticToken("t"), func(attempt int, w http.ResponseWriter, _ *http.Request) {
			abortHandler()(w, nil)
		})
		if err := client.Ready(ctx); err == nil {
			t.Fatalf("Ready() error = nil, want the transport failure")
		}
		if got := atomic.LoadInt32(count); got != 1 {
			t.Fatalf("attempts = %d, want 1 (a health probe fails fast)", got)
		}
	})
}

// refreshTokenSource records token and forced-refresh calls.
type refreshTokenSource struct {
	mu           sync.Mutex
	token        string
	refreshed    string
	tokenCalls   int
	refreshCalls int
}

func newRefreshTokenSource(stale, fresh string) *refreshTokenSource {
	return &refreshTokenSource{token: stale, refreshed: fresh}
}

func (s *refreshTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenCalls++
	return s.token, nil
}

func (s *refreshTokenSource) Refresh(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls++
	s.token = s.refreshed
	return s.refreshed, nil
}

func (s *refreshTokenSource) calls() (tokenCalls, refreshCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenCalls, s.refreshCalls
}

func unauthorizedUnlessFresh(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fresh" {
		jsonHandler(http.StatusUnauthorized, `{"detail":"invalid_token"}`)(w, r)
		return
	}
	jsonHandler(http.StatusOK, `{"id":"dev1"}`)(w, r)
}

// TestForcedTokenRefreshOn401 pins the "one forced refresh, one retry" rule.
func TestForcedTokenRefreshOn401(t *testing.T) {
	ctx := context.Background()

	t.Run("refresher source recovers", func(t *testing.T) {
		tokens := newRefreshTokenSource("stale", "fresh")
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, r *http.Request) {
			unauthorizedUnlessFresh(w, r)
		})
		if _, err := client.Device(ctx, "dev1"); err != nil {
			t.Fatalf("Device() error = %v, want the forced refresh to recover", err)
		}
		captured := requests.all()
		if len(captured) != 2 {
			t.Fatalf("requests = %d, want 2 (original plus one retry)", len(captured))
		}
		if captured[1].authorization != "Bearer fresh" {
			t.Fatalf("retry Authorization = %q, want the refreshed token", captured[1].authorization)
		}
		tokenCalls, refreshCalls := tokens.calls()
		if tokenCalls != 1 || refreshCalls != 1 {
			t.Fatalf("token calls = %d, refresh calls = %d; want 1 and 1", tokenCalls, refreshCalls)
		}
	})

	t.Run("still rejected after the refresh", func(t *testing.T) {
		tokens := newRefreshTokenSource("stale", "stale")
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusUnauthorized, `{"detail":"invalid_token"}`)(w, r)
		})
		if _, err := client.Device(ctx, "dev1"); !IsStatus(err, http.StatusUnauthorized) {
			t.Fatalf("Device() error = %v, want a 401 UpstreamError", err)
		}
		if got := len(requests.all()); got != 2 {
			t.Fatalf("requests = %d, want exactly 2", got)
		}
		if _, refreshCalls := tokens.calls(); refreshCalls != 1 {
			t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
		}
	})

	t.Run("source without a refresher retries once", func(t *testing.T) {
		var tokenCalls int32
		tokens := TokenSourceFunc(func(context.Context) (string, error) {
			atomic.AddInt32(&tokenCalls, 1)
			return "stale", nil
		})
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, r *http.Request) {
			jsonHandler(http.StatusUnauthorized, `{"detail":"invalid_token"}`)(w, r)
		})
		if _, err := client.Device(ctx, "dev1"); !IsStatus(err, http.StatusUnauthorized) {
			t.Fatalf("Device() error = %v, want a 401 UpstreamError", err)
		}
		if got := len(requests.all()); got != 2 {
			t.Fatalf("requests = %d, want exactly 2 (never a loop)", got)
		}
		if got := atomic.LoadInt32(&tokenCalls); got != 2 {
			t.Fatalf("token calls = %d, want 2", got)
		}
	})

	t.Run("single-attempt commands refresh too", func(t *testing.T) {
		tokens := newRefreshTokenSource("stale", "fresh")
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fresh" {
				jsonHandler(http.StatusUnauthorized, `{"detail":"invalid_token"}`)(w, r)
				return
			}
			jsonHandler(http.StatusAccepted, `{"command_id":"cmd1","request_id":"req1"}`)(w, r)
		})
		ack, err := client.TakePhoto(ctx, "dev1", 0)
		if err != nil || ack.RequestID != "req1" {
			t.Fatalf("TakePhoto() = %+v, err %v; want the refreshed retry to succeed", ack, err)
		}
		if got := len(requests.all()); got != 2 {
			t.Fatalf("requests = %d, want 2", got)
		}
	})

	t.Run("token source failure never reaches the network", func(t *testing.T) {
		tokens := TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("no credential")
		})
		client, requests, _ := newAttemptServer(t, tokens, func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := client.Device(ctx, "dev1")
		if err == nil || !strings.Contains(err.Error(), "obtain service token") {
			t.Fatalf("Device() error = %v, want the token failure", err)
		}
		if got := len(requests.all()); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	})

	t.Run("nil token source is rejected locally", func(t *testing.T) {
		server := newTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, err := server.client.Device(ctx, "dev1")
		if err == nil || !strings.Contains(err.Error(), "no token source") {
			t.Fatalf("Device() error = %v, want the missing-token-source failure", err)
		}
		if got := len(server.rec.all()); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	})
}

// TestResponseCap rejects an oversized upstream body instead of truncating it.
func TestResponseCap(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, strings.Repeat("a", maxResponseBytes+1)))
	_, err := server.client.Device(context.Background(), "dev1")
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("Device() error = %v, want the response cap failure", err)
	}
	if got := len(server.rec.all()); got != 1 {
		t.Fatalf("attempts = %d, want 1 (a capped response is not retried)", got)
	}
}

// TestDecodeFailureIsReported keeps a malformed 2xx body from silently
// producing an empty result.
func TestDecodeFailureIsReported(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusOK, `{"id":`))
	_, err := server.client.Device(context.Background(), "dev1")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("Device() error = %v, want a decode failure", err)
	}
}

// TestStartWithoutStreamID covers the guard that makes a broken upstream
// response fail loudly instead of persisting an empty handle.
func TestStartWithoutStreamID(t *testing.T) {
	server := newTestServer(t, staticToken("t"), jsonHandler(http.StatusCreated, `{}`))
	_, err := server.client.Start(context.Background(), "dev1", 0)
	if err == nil || !strings.Contains(err.Error(), "without a stream id") {
		t.Fatalf("Start() error = %v, want the missing stream id failure", err)
	}
}
