// Package apiclient is the typed brokerctl -> broker HTTP+WebSocket client. It
// decodes the frozen internal/api wire types and surfaces HTTP errors as a typed
// StatusError so callers (internal/cli) can map status codes to exit codes — the
// degradation switch keys on 501 (reserved route, owning epic not landed), never
// 404. The Authorization: Bearer header is set on every request and the WS upgrade.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/papanton/bazel-broker/internal/api"
)

// Client talks to the broker over loopback HTTP+WS with a bearer token.
type Client struct {
	base   string // "http://127.0.0.1:8765"
	wsBase string // "ws://127.0.0.1:8765"
	token  string
	http   *http.Client
}

// New constructs a client for host:port with the given bearer token and HTTP
// client (the caller supplies the timeout; pass a no-timeout client for watch).
func New(host string, port int, token string, hc *http.Client) *Client {
	if host == "" {
		host = "127.0.0.1"
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	authority := net.JoinHostPort(host, strconv.Itoa(port))
	return &Client{
		base:   "http://" + authority,
		wsBase: "ws://" + authority,
		token:  token,
		http:   hc,
	}
}

// BaseURL returns the HTTP base (e.g. "http://127.0.0.1:8765").
func (c *Client) BaseURL() string { return c.base }

// StatusError is returned for any non-2xx HTTP response. It carries the status
// code (so callers can branch on 401/501/404/…) and the broker's decoded
// {"error","message","epic"} detail.
type StatusError struct {
	Method  string
	Path    string
	Status  int
	Code    string // broker machine code ("not_implemented", "not_found", …)
	Message string // broker human message
	Epic    string // owning epic for 501 reserved routes
}

func (e *StatusError) Error() string {
	if d := e.Detail(); d != "" {
		return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, d)
	}
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// Detail renders the human tail of the error: the broker's message (or code),
// plus the owning epic when the message doesn't already name it (the 501 body
// sets both Message="owned by E3" and Epic="E3"). Empty when the broker sent no
// detail. This is the single formatter shared by Error() and the CLI's exit-code
// mapping so the two never drift.
func (e *StatusError) Detail() string {
	detail := e.Message
	if detail == "" {
		detail = e.Code
	}
	if e.Epic != "" && !strings.Contains(detail, e.Epic) {
		detail = strings.TrimSpace(detail + " (owner " + e.Epic + ")")
	}
	return detail
}

// TransportError wraps a connection-level failure (refused/timeout/DNS).
type TransportError struct {
	Method string
	Path   string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("%s %s: broker unreachable: %v", e.Method, e.Path, e.Err)
}
func (e *TransportError) Unwrap() error { return e.Err }

// do performs an HTTP request with bearer auth, returning a StatusError for any
// non-2xx and a TransportError for connection failures.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s body: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &TransportError{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return newStatusError(method, path, resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// newStatusError decodes the broker's error body into a StatusError.
func newStatusError(method, path string, resp *http.Response) *StatusError {
	se := &StatusError{Method: method, Path: path, Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e api.ErrorResponse
	if json.Unmarshal(b, &e) == nil {
		se.Code, se.Message, se.Epic = e.Error, e.Message, e.Epic
	}
	if se.Code == "" && se.Message == "" {
		se.Message = strings.TrimSpace(string(b))
	}
	return se
}

// --- typed methods (paths verbatim from the frozen contract, api §4.2) ---

// Healthz fetches GET /healthz.
func (c *Client) Healthz(ctx context.Context) (api.HealthResponse, error) {
	var h api.HealthResponse
	return h, c.do(ctx, http.MethodGet, "/healthz", nil, &h)
}

// ListBuilds fetches GET /builds → the full {"builds":[…]} envelope.
func (c *Client) ListBuilds(ctx context.Context) (api.BuildsResponse, error) {
	var r api.BuildsResponse
	return r, c.do(ctx, http.MethodGet, "/builds", nil, &r)
}

// GetBuild fetches GET /builds/{invocation_id}.
func (c *Client) GetBuild(ctx context.Context, id string) (api.Build, error) {
	var r api.BuildResponse
	return r.Build, c.do(ctx, http.MethodGet, "/builds/"+id, nil, &r)
}

// Kill issues POST /builds/{invocation_id}/kill (E3; 501 until E3).
func (c *Client) Kill(ctx context.Context, id string) (api.KillResult, error) {
	var res api.KillResult
	return res, c.do(ctx, http.MethodPost, "/builds/"+id+"/kill", nil, &res)
}

// Drain issues POST /admission/drain (E5; 501 until E5).
func (c *Client) Drain(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/admission/drain", nil, nil)
}

// Pause issues POST /admission/pause (E5; 501 until E5).
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/admission/pause", nil, nil)
}

// Resume issues POST /admission/resume (E5; 501 until E5).
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/admission/resume", nil, nil)
}

// Profile fetches GET /builds/{invocation_id}/profile → api.ProfileRef (E4; 501
// until E4).
func (c *Client) Profile(ctx context.Context, id string) (api.ProfileRef, error) {
	var p api.ProfileRef
	return p, c.do(ctx, http.MethodGet, "/builds/"+id+"/profile", nil, &p)
}

// --- WebSocket event stream ---

// EventStream is a live WS connection to GET /events.
type EventStream struct{ conn *websocket.Conn }

// StreamEvents dials GET /events with the bearer header. A 401 on the upgrade
// surfaces as a *StatusError{Status:401}; any other dial failure as *TransportError.
func (c *Client) StreamEvents(ctx context.Context) (*EventStream, error) {
	conn, resp, err := websocket.Dial(ctx, c.wsBase+"/events", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return nil, newStatusError(http.MethodGet, "/events", resp)
		}
		return nil, &TransportError{Method: http.MethodGet, Path: "/events", Err: err}
	}
	return &EventStream{conn: conn}, nil
}

// Next blocks for the next event frame. Returns the underlying read error
// (io.EOF / close) so the caller can decide reconnect vs exit.
func (s *EventStream) Next(ctx context.Context) (api.Event, error) {
	var ev api.Event
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return ev, err
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return ev, fmt.Errorf("decode event frame: %w", err)
	}
	return ev, nil
}

// Close closes the WS connection with a normal-closure status.
func (s *EventStream) Close() { _ = s.conn.Close(websocket.StatusNormalClosure, "bye") }
