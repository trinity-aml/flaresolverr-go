package flaresolverr

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

type contextKeyV1Request struct{}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(payload)
}

type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *captureWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(payload)
	return w.ResponseWriter.Write(payload)
}

func requestWithV1Body(r *http.Request) *http.Request {
	if r.URL.Path != "/v1" || r.Method != http.MethodPost || r.Body == nil {
		return r
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return r
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req V1Request
	if err := json.Unmarshal(body, &req); err != nil {
		return r
	}
	ctx := context.WithValue(r.Context(), contextKeyV1Request{}, &req)
	return r.WithContext(ctx)
}

func v1RequestFromContext(ctx context.Context) *V1Request {
	value, _ := ctx.Value(contextKeyV1Request{}).(*V1Request)
	return value
}
