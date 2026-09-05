// Package client is a thin HTTP client for the trackd API, used by the CLI.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds a single attempt. It is deliberately short: every
// endpoint is a local SQLite read or write, so a slow response means the
// server is wedged and retrying beats waiting.
const requestTimeout = 10 * time.Second

// retryDelays are the waits before the second, third and fourth attempts.
var retryDelays = []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second}

type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
	delays  []time.Duration
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpc:   &http.Client{Timeout: requestTimeout},
		delays:  retryDelays,
	}
}

// APIError is a non-2xx response from the server. Code is the short machine
// string from the error body ("not_found", "conflict", "invalid_ref", ...); a
// response carrying no usable JSON body reports "internal".
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d %s)", e.Message, e.Status, e.Code)
}

// TransportError is a failure to reach the server at all: connection refused,
// a timeout, a dropped connection. Retries happen underneath it, so seeing one
// means every attempt failed.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// Do sends one API request, marshalling body as JSON and decoding the response
// into out. Both may be nil.
func (c *Client) Do(method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	// A POST creates something. Repeating one without an idempotency key could
	// create it twice, so only POSTs carrying a key are retried; every other
	// method is idempotent by contract.
	repeatable := method != http.MethodPost || hasIdempotencyKey(payload)
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			time.Sleep(c.delays[attempt-1])
		}
		err := c.attempt(method, u, payload, out)
		if err == nil {
			return nil
		}
		if !repeatable || !worthRetrying(err) || attempt == len(c.delays) {
			return err
		}
	}
}

// attempt performs one HTTP round trip.
func (c *Client) attempt(method, u string, payload []byte, out any) error {
	var reader io.Reader = http.NoBody
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return &TransportError{Op: method + " " + u, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return readAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding the response to %s: %w", u, err)
	}
	return nil
}

// worthRetrying reports whether another attempt could plausibly succeed: the
// server said it was busy, or we never reached it.
func worthRetrying(err error) bool {
	switch e := err.(type) {
	case *APIError:
		return e.Status == http.StatusServiceUnavailable
	case *TransportError:
		return true
	}
	return false
}

// getOnce performs a single unretried GET and decodes the body whatever the
// status was, returning that status alongside. It exists for /healthz, which
// answers 503 with the report that explains why.
func (c *Client) getOnce(path string, out any) (int, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, http.NoBody)
	if err != nil {
		return 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, &TransportError{Op: "GET " + c.baseURL + path, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, apiErrorFrom(resp.StatusCode, resp.Status, body)
	}
	return resp.StatusCode, nil
}

// readAPIError turns an error response into an APIError.
func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return apiErrorFrom(resp.StatusCode, resp.Status, body)
}

// apiErrorFrom reads the error contract. The server always sends
// {"error": ..., "code": ...}; anything else (a proxy's HTML page, an empty
// body) is reported as an internal error carrying the status line.
func apiErrorFrom(status int, statusText string, body []byte) error {
	e := &APIError{Status: status, Code: "internal", Message: strings.TrimSpace(statusText)}
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		e.Message = payload.Error
		if payload.Code != "" {
			e.Code = payload.Code
		}
	}
	return e
}

// hasIdempotencyKey reports whether a request body carries a non-empty
// idempotency key, which is what makes a create safe to repeat.
func hasIdempotencyKey(payload []byte) bool {
	var probe struct {
		Key string `json:"idempotency_key"`
	}
	return json.Unmarshal(payload, &probe) == nil && probe.Key != ""
}
