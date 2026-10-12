package scheduler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/merge"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// TestMergeDestKey pins the destination object layout:
// merged/<room_code>/<starts_at as the local calendar day>/<session_id>.mp4.
// The date must come from the configured zone, not UTC, so a recording that
// starts late in the evening lands under its local day.
func TestMergeDestKey(t *testing.T) {
	start := time.Date(2026, 10, 12, 22, 30, 0, 0, time.UTC)
	tests := []struct {
		name      string
		location  *time.Location
		roomCode  string
		sessionID string
		startsAt  time.Time
		want      string
	}{
		{
			name:      "utc",
			location:  time.UTC,
			roomCode:  "A301",
			sessionID: "01J8Z000000000000000000000",
			startsAt:  start,
			want:      "merged/A301/2026-10-12/01J8Z000000000000000000000.mp4",
		},
		{
			// 22:30 UTC is already the next day in Shanghai (+08:00).
			name:      "non-utc zone shifts the calendar day",
			location:  time.FixedZone("CST", 8*3600),
			roomCode:  "A301",
			sessionID: "01J8Z000000000000000000000",
			startsAt:  start,
			want:      "merged/A301/2026-10-13/01J8Z000000000000000000000.mp4",
		},
		{
			// A negative offset moves the date backwards.
			name:      "negative offset",
			location:  time.FixedZone("EST", -5*3600),
			roomCode:  "B-102",
			sessionID: "abc",
			startsAt:  start,
			want:      "merged/B-102/2026-10-12/abc.mp4",
		},
		{
			// A room code with no slash keeps the key segments intact.
			name:      "slash-free room code stays one segment",
			location:  time.UTC,
			roomCode:  "A301-EAST",
			sessionID: "s1",
			startsAt:  start,
			want:      "merged/A301-EAST/2026-10-12/s1.mp4",
		},
		{
			// A nil location must not panic; it falls back to UTC.
			name:      "nil location falls back to utc",
			location:  nil,
			roomCode:  "A301",
			sessionID: "s1",
			startsAt:  start,
			want:      "merged/A301/2026-10-12/s1.mp4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeDestKey(tt.location, tt.roomCode, tt.sessionID, tt.startsAt)
			if got != tt.want {
				t.Fatalf("mergeDestKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRejectedMergeCodec pins the pre-submit codec guard. smartclass-merge has
// no raw VP8/VP9 bitstream demuxer, so those recordings are refused before the
// submission instead of failing upstream.
func TestRejectedMergeCodec(t *testing.T) {
	tests := []struct {
		name       string
		meta       webcam.StreamMetadata
		wantCodec  string
		wantReject bool
	}{
		{
			name:       "empty metadata lets merge sniff",
			meta:       webcam.StreamMetadata{},
			wantReject: false,
		},
		{
			name:       "settled h264 is mergeable",
			meta:       webcam.StreamMetadata{Codec: "h264"},
			wantReject: false,
		},
		{
			name:       "settled vp8 is rejected",
			meta:       webcam.StreamMetadata{Codec: "vp8"},
			wantCodec:  "vp8",
			wantReject: true,
		},
		{
			name:       "settled vp9 is rejected case-insensitively",
			meta:       webcam.StreamMetadata{Codec: "vp9"},
			wantCodec:  "vp9",
			wantReject: true,
		},
		{
			name:       "uppercase settled codec is normalized",
			meta:       webcam.StreamMetadata{Codec: " VP9 "},
			wantCodec:  "vp9",
			wantReject: true,
		},
		{
			// The settled codec decides when present: an advertised muxable
			// codec alongside does not save a stream whose recorder picked VP9.
			name:       "settled codec overrides the advertised list",
			meta:       webcam.StreamMetadata{Codec: "vp9", Codecs: []string{"h264"}},
			wantCodec:  "vp9",
			wantReject: true,
		},
		{
			// Without a settled codec, a stream offering any muxable codec is
			// submitted: merge can pick the muxable one.
			name:       "advertised h264 is enough to submit",
			meta:       webcam.StreamMetadata{Codecs: []string{"vp8", "h264"}},
			wantReject: false,
		},
		{
			name:       "all advertised codecs unmuxable is rejected",
			meta:       webcam.StreamMetadata{Codecs: []string{"VP8", "vp9"}},
			wantCodec:  "vp8",
			wantReject: true,
		},
		{
			name:       "empty advertised entries are ignored",
			meta:       webcam.StreamMetadata{Codecs: []string{"", "vp8"}},
			wantCodec:  "vp8",
			wantReject: true,
		},
		{
			name:       "blank advertised entries alone do not reject",
			meta:       webcam.StreamMetadata{Codecs: []string{"", "  "}},
			wantReject: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			codec, rejected := rejectedMergeCodec(tt.meta)
			if rejected != tt.wantReject {
				t.Fatalf("rejectedMergeCodec() rejected = %v, want %v", rejected, tt.wantReject)
			}
			if codec != tt.wantCodec {
				t.Fatalf("rejectedMergeCodec() codec = %q, want %q", codec, tt.wantCodec)
			}
		})
	}
}

// TestPermanentMergeSubmit pins the classification of a submission failure: a
// malformed request (400) or a credential without merge:write:any (403) would
// repeat for the same body, so it is permanent; a transport failure, a 5xx or
// the retryable 503 source_unavailable is transient.
func TestPermanentMergeSubmit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "400 invalid_request is permanent",
			err:  &merge.UpstreamError{Status: http.StatusBadRequest, Detail: "invalid_request"},
			want: true,
		},
		{
			name: "403 insufficient_permissions is permanent",
			err:  &merge.UpstreamError{Status: http.StatusForbidden, Detail: "insufficient_permissions"},
			want: true,
		},
		{
			name: "409 status_not_cancelable is transient",
			err:  &merge.UpstreamError{Status: http.StatusConflict, Detail: "status_not_cancelable"},
			want: false,
		},
		{
			name: "503 source_unavailable is transient",
			err:  &merge.UpstreamError{Status: http.StatusServiceUnavailable, Detail: "source_unavailable"},
			want: false,
		},
		{
			name: "500 is transient",
			err:  &merge.UpstreamError{Status: http.StatusInternalServerError},
			want: false,
		},
		{
			name: "transport failure is transient",
			err:  errors.New("merge: create: connection refused"),
			want: false,
		},
		{
			name: "no error is not permanent",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permanentMergeSubmit(tt.err); got != tt.want {
				t.Fatalf("permanentMergeSubmit(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestMergeJobMissing pins the 404 classification that ends a submitted row:
// merge no longer knows the job, so polling it forever is pointless.
func TestMergeJobMissing(t *testing.T) {
	if !mergeJobMissing(&merge.UpstreamError{Status: http.StatusNotFound, Detail: "job_not_found"}) {
		t.Fatal("mergeJobMissing(404) = false, want true")
	}
	if mergeJobMissing(&merge.UpstreamError{Status: http.StatusServiceUnavailable, Detail: "source_unavailable"}) {
		t.Fatal("mergeJobMissing(503) = true, want false (transient)")
	}
	if mergeJobMissing(errors.New("merge: get: connection refused")) {
		t.Fatal("mergeJobMissing(transport) = true, want false (transient)")
	}
}

// TestMergeDetail pins the operator-facing detail: the RFC 9457 detail when
// there is one, the error text otherwise and "" for no error.
func TestMergeDetail(t *testing.T) {
	if got := mergeDetail(nil); got != "" {
		t.Fatalf("mergeDetail(nil) = %q, want empty", got)
	}
	if got := mergeDetail(&merge.UpstreamError{Status: http.StatusBadRequest, Detail: "invalid_request"}); got != "invalid_request" {
		t.Fatalf("mergeDetail(problem) = %q, want the RFC 9457 detail", got)
	}
	cause := errors.New("merge: get: connection refused")
	if got := mergeDetail(cause); got != cause.Error() {
		t.Fatalf("mergeDetail(transport) = %q, want the error text", got)
	}
}
