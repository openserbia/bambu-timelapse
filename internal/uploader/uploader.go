// Package uploader posts a finished timelapse to a media API.
package uploader

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

const (
	// uploadTimeout covers the whole POST. A several-hundred-megabyte video
	// over a home uplink is minutes, not seconds.
	uploadTimeout = 30 * time.Minute
	// errorBodyLimit bounds how much of a failure response is read back.
	errorBodyLimit = 64 << 10
	// boundaryBytes of randomness makes an accidental collision with the
	// video's own bytes impossible in practice.
	boundaryBytes = 16
	// serverErrorFloor is where "our payload is wrong" ends and "try again
	// later" begins.
	serverErrorFloor = 500
)

// Error carries whether another attempt could plausibly succeed.
type Error struct {
	Status    int
	Body      string
	Retryable bool
	err       error
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("media api: %v", e.err)
	}
	return fmt.Sprintf("media api: HTTP %d: %s", e.Status, e.Body)
}

func (e *Error) Unwrap() error { return e.err }

// Request is one timelapse post.
type Request struct {
	VideoPath string
	CoverPath string // optional
	Filename  string
	Caption   string
	Duration  int
	Width     int
	Height    int
}

// Uploader posts multipart/form-data to the media API.
type Uploader struct {
	url    string
	token  string
	fields map[string]string
	client *http.Client
}

// New builds an Uploader. fields are posted verbatim with every video and are
// never interpreted here. They carry the consumer's routing, which is not
// this service's business.
//
// The timeout covers the whole upload, which for a several-hundred-megabyte
// video over a home uplink is minutes, not seconds.
func New(apiURL, token string, fields map[string]string) *Uploader {
	return &Uploader{
		url: apiURL, token: token, fields: fields,
		client: &http.Client{Timeout: uploadTimeout},
	}
}

// Post uploads one video and returns the Telegram message ids.
//
// The body is assembled by hand rather than with mime/multipart's Writer
// because two properties matter: the 'video' part must come LAST (the
// endpoint streams it and stops reading there), and the file must stream off
// disk rather than be buffered, since this runs on a Pi beside the rest of a
// stack. An io.Pipe would satisfy the first but forces chunked encoding; the
// exact Content-Length computed here lets the server size the request up
// front.
func (u *Uploader) Post(ctx context.Context, req Request) ([]int, error) {
	boundary, err := randomBoundary()
	if err != nil {
		return nil, &Error{err: err, Retryable: true}
	}

	video, err := os.Open(req.VideoPath)
	if err != nil {
		return nil, &Error{err: err, Retryable: false}
	}
	defer func() { _ = video.Close() }()

	info, err := video.Stat()
	if err != nil {
		return nil, &Error{err: err, Retryable: false}
	}

	prologue := u.prologue(boundary, req)
	epilogue := []byte("\r\n--" + boundary + "--\r\n")

	body := io.MultiReader(bytes.NewReader(prologue), video, bytes.NewReader(epilogue))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, body)
	if err != nil {
		return nil, &Error{err: err, Retryable: false}
	}
	httpReq.Header.Set("Authorization", "Bearer "+u.token)
	httpReq.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	httpReq.ContentLength = int64(len(prologue)) + info.Size() + int64(len(epilogue))

	resp, err := u.client.Do(httpReq)
	if err != nil {
		// Transport failures are the retryable kind: the endpoint's host
		// rebooting, the link flapping, DNS blinking.
		return nil, &Error{err: err, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{
			Status: resp.StatusCode,
			Body:   string(payload),
			// 4xx means our payload is wrong and every retry fails identically.
			// 5xx (including the 502 the endpoint returns when Telegram itself
			// rejected the send) is worth another attempt.
			Retryable: resp.StatusCode >= serverErrorFloor,
		}
	}

	var decoded struct {
		Data struct {
			MessageIDs []int `json:"message_ids"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, &Error{err: err, Retryable: false}
	}
	return decoded.Data.MessageIDs, nil
}

// prologue renders every scalar field, then the optional cover, then the
// header of the video part: everything up to the file's bytes.
func (u *Uploader) prologue(boundary string, req Request) []byte {
	var buf bytes.Buffer

	field := func(name, value string) {
		fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n",
			boundary, name, value)
	}

	// Caller-supplied routing first, so a field the service owns below can
	// never be silently overridden by configuration.
	for _, name := range sortedKeys(u.fields) {
		field(name, u.fields[name])
	}

	field("caption", req.Caption)
	field("filename", req.Filename)
	field("duration", strconv.Itoa(req.Duration))
	field("width", strconv.Itoa(req.Width))
	field("height", strconv.Itoa(req.Height))
	field("no_audio", "true")

	if req.CoverPath != "" {
		// #nosec G304 -- the cover is a frame this service captured.
		cover, err := os.ReadFile(req.CoverPath)
		if err == nil && len(cover) > 0 {
			fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=\"thumbnail\"; "+
				"filename=\"cover.jpg\"\r\nContent-Type: image/jpeg\r\n\r\n", boundary)
			buf.Write(cover)
			buf.WriteString("\r\n")
		}
	}

	// 'video' last, by the endpoint's contract.
	fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=\"video\"; "+
		"filename=%q\r\nContent-Type: video/mp4\r\n\r\n", boundary, req.Filename)
	return buf.Bytes()
}

// sortedKeys keeps the multipart body deterministic, which makes the request
// reproducible and its tests stable.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func randomBoundary() (string, error) {
	var b [boundaryBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "----bambu" + hex.EncodeToString(b[:]), nil
}
