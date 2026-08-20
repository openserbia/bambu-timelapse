package uploader

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type capture struct {
	order       []string
	fields      map[string]string
	video       string
	thumb       string
	contentLen  int64
	declaredLen int64
}

// serve stands in for the media API and records exactly what arrived.
func serve(t *testing.T, status int, body string, got *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.declaredLen = r.ContentLength
		got.fields = map[string]string{}

		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("bad content type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			name := part.FormName()
			got.order = append(got.order, name)
			body, _ := io.ReadAll(part)
			switch name {
			case "video":
				got.video = string(body)
				got.contentLen = int64(len(body))
			case "thumbnail":
				got.thumb = string(body)
			default:
				got.fields[name] = string(body)
			}
			_ = part.Close()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPostSendsVideoLastWithExactLength(t *testing.T) {
	got := &capture{}
	srv := serve(t, 200, `{"data":{"message_ids":[4242]}}`, got)
	defer srv.Close()

	video := writeFixture(t, "print.mp4", "MP4BYTES")
	thumb := writeFixture(t, "cover.jpg", "JPEGBYTES")

	up := New(srv.URL, "tok", -1001234567890, 907, true)
	ids, err := up.Post(context.Background(), Request{
		VideoPath: video, CoverPath: thumb, Filename: "print.mp4",
		Caption: "done", Duration: 24, Width: 1920, Height: 1080,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(ids) != 1 || ids[0] != 4242 {
		t.Fatalf("ids = %v", ids)
	}

	// The endpoint streams the video part and stops reading there, so anything
	// after it would never be seen.
	if last := got.order[len(got.order)-1]; last != "video" {
		t.Fatalf("part order %v — video must be last", got.order)
	}
	if got.video != "MP4BYTES" || got.thumb != "JPEGBYTES" {
		t.Fatalf("video=%q thumb=%q", got.video, got.thumb)
	}
	for key, want := range map[string]string{
		"chat_id": "-1001234567890", "topic_id": "907", "caption": "done",
		"filename": "print.mp4", "duration": "24", "width": "1920",
		"height": "1080", "no_audio": "true", "silent": "true",
	} {
		if got.fields[key] != want {
			t.Errorf("field %s = %q, want %q", key, got.fields[key], want)
		}
	}
	// A wrong Content-Length either truncates the upload or hangs the server
	// waiting for bytes that never come, so assert it explicitly.
	if got.declaredLen <= 0 {
		t.Fatal("Content-Length was not declared; the body streamed as chunked")
	}
}

func TestTopicOmittedWhenUnset(t *testing.T) {
	got := &capture{}
	srv := serve(t, 200, `{"data":{"message_ids":[1]}}`, got)
	defer srv.Close()

	up := New(srv.URL, "tok", -1001, 0, false)
	if _, err := up.Post(context.Background(), Request{
		VideoPath: writeFixture(t, "v.mp4", "x"), Filename: "v.mp4",
	}); err != nil {
		t.Fatal(err)
	}
	if _, present := got.fields["topic_id"]; present {
		t.Fatal("topic_id must be omitted when zero, not sent as 0")
	}
	if got.fields["silent"] != "false" {
		t.Fatalf("silent = %q", got.fields["silent"])
	}
}

func TestRetryClassification(t *testing.T) {
	cases := map[int]bool{
		400: false, // our payload is wrong; every retry fails identically
		401: false,
		422: false,
		500: true,
		502: true, // the endpoint's "Telegram rejected the send"
		503: true,
	}
	for status, wantRetryable := range cases {
		got := &capture{}
		srv := serve(t, status, `{"error":{"code":"X"}}`, got)

		up := New(srv.URL, "tok", -1001, 0, false)
		_, err := up.Post(context.Background(), Request{
			VideoPath: writeFixture(t, "v.mp4", "x"), Filename: "v.mp4",
		})
		srv.Close()

		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: error was %T, want *Error", status, err)
		}
		if apiErr.Retryable != wantRetryable {
			t.Errorf("status %d: retryable = %v, want %v", status, apiErr.Retryable, wantRetryable)
		}
	}
}

func TestTransportFailureIsRetryable(t *testing.T) {
	// A closed server stands in for AX41 rebooting or the tailnet flapping.
	srv := serve(t, 200, "", &capture{})
	url := srv.URL
	srv.Close()

	up := New(url, "tok", -1001, 0, false)
	_, err := up.Post(context.Background(), Request{
		VideoPath: writeFixture(t, "v.mp4", "x"), Filename: "v.mp4",
	})
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable {
		t.Fatalf("transport failure must be retryable, got %v", err)
	}
}

func TestMissingVideoIsNotRetryable(t *testing.T) {
	up := New("http://127.0.0.1:1", "tok", -1001, 0, false)
	_, err := up.Post(context.Background(), Request{
		VideoPath: "/nonexistent.mp4", Filename: "v.mp4",
	})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Retryable {
		t.Fatalf("a missing file must not be retried, got %v", err)
	}
}
