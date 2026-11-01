// Package httpx has the small JSON-over-HTTP helpers both sides share.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"aicluster/common/proto"
)

const maxBody = 4 << 20

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, proto.ErrorBody{V: proto.Version, Error: proto.ErrorInfo{
		Code: code, Message: msg, Retryable: status >= 500 || status == http.StatusConflict,
	}})
}

// ReadJSON decodes a size-limited request body into v.
func ReadJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(v)
}

// APIError is returned by Do when the peer answered with an error envelope.
type APIError struct {
	Status int
	Info   proto.ErrorInfo
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Info.Code, e.Info.Message)
}

// Code returns the error code if err is an *APIError, else "".
func Code(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Info.Code
	}
	return ""
}

// Do sends in (if non-nil) as JSON and decodes a 2xx response into out (if
// non-nil). Non-2xx responses become *APIError.
func Do(ctx context.Context, c *http.Client, method, url string, in, out any, header ...string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var eb proto.ErrorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error.Code != "" {
			return &APIError{Status: resp.StatusCode, Info: eb.Error}
		}
		return &APIError{Status: resp.StatusCode, Info: proto.ErrorInfo{Code: "http_" + fmt.Sprint(resp.StatusCode), Message: string(bytes.TrimSpace(data))}}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
