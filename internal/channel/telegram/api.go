package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

// maxErrorBody caps how much of a non-JSON error page is read.
const maxErrorBody = 32 << 10

// endpoint builds the method URL. The token sits in the path, which is why
// every error out of this package is scrubbed.
func (a *Adapter) endpoint(token, method string) string {
	return a.base + "/bot" + token + "/" + method
}

// call invokes a Bot API method with a JSON body and decodes result into out.
func (a *Adapter) call(ctx context.Context, method string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{Method: method, Kind: KindBadRequest, Description: "encode request"}
		}
		rdr = bytes.NewReader(buf)
	}
	token := a.token()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(token, method), rdr)
	if err != nil {
		return &Error{Method: method, Kind: KindBadRequest, Description: scrub(err.Error(), token)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.do(ctx, method, token, req, out)
}

// do runs req and decodes the Bot API envelope.
func (a *Adapter) do(ctx context.Context, method, token string, req *http.Request, out any) error {
	resp, err := a.hc.Do(req)
	if err != nil {
		return a.transportError(ctx, method, token, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return a.transportError(ctx, method, token, err)
	}
	var env apiResponse
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		if resp.StatusCode/100 == 2 {
			return &Error{Method: method, Kind: KindServer, Status: resp.StatusCode, Description: "malformed response"}
		}
		env.Description = scrub(string(raw), token)
	}
	if !env.OK || resp.StatusCode/100 != 2 {
		e := &Error{
			Method:      method,
			Kind:        kindOf(resp.StatusCode),
			Status:      resp.StatusCode,
			Description: scrub(env.Description, token),
		}
		if env.Parameters != nil && env.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(env.Parameters.RetryAfter) * time.Second
			e.Kind = KindRateLimit
		}
		return e
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return &Error{Method: method, Kind: KindServer, Status: resp.StatusCode, Description: "malformed result"}
		}
	}
	return nil
}

// transportError turns a transport failure into a scrubbed *Error. The cause
// is unwrapped out of *url.Error first: that wrapper quotes the request URL,
// and the URL carries the token.
func (a *Adapter) transportError(ctx context.Context, method, token string, err error) error {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err
	}
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	return &Error{
		Method:      method,
		Kind:        KindNetwork,
		Description: scrub(cause.Error(), token),
		cause:       cause,
	}
}

// multipartFile is one streamed upload field.
type multipartFile struct {
	Field string
	Name  string
	MIME  string
	Data  []byte
}

// upload posts a multipart form, streaming the file body through a pipe so a
// large blob is never copied into a second buffer.
func (a *Adapter) upload(ctx context.Context, method string, fields map[string]string, file multipartFile, out any) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		defer func() { _ = pw.CloseWithError(err) }()
		for k, v := range fields {
			if err = mw.WriteField(k, v); err != nil {
				return
			}
		}
		var w io.Writer
		if w, err = mw.CreateFormFile(file.Field, file.Name); err != nil {
			return
		}
		if _, err = io.Copy(w, bytes.NewReader(file.Data)); err != nil {
			return
		}
		err = mw.Close()
	}()
	defer func() { _ = pr.Close() }()

	token := a.token()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(token, method), pr)
	if err != nil {
		return &Error{Method: method, Kind: KindBadRequest, Description: scrub(err.Error(), token)}
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return a.do(ctx, method, token, req, out)
}

// download fetches a file by its Bot API path. It reads at most limit bytes and
// reports whether the file was longer, which the caller turns into a note
// instead of an attachment (docs/sessions.md).
func (a *Adapter) download(ctx context.Context, path string, limit int64) ([]byte, bool, error) {
	token := a.token()
	u := a.base + "/file/bot" + token + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, &Error{Method: "download", Kind: KindBadRequest, Description: scrub(err.Error(), token)}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, false, a.transportError(ctx, "download", token, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, false, &Error{
			Method: "download", Kind: kindOf(resp.StatusCode), Status: resp.StatusCode,
			Description: "file download failed",
		}
	}
	// One byte past the cap tells an oversized file from an exact fit.
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, a.transportError(ctx, "download", token, err)
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}
