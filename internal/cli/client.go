package cli

import (
	"context"
	"errors"
	"net/http"

	"github.com/papanton/bazel-broker/internal/api"
	"github.com/papanton/bazel-broker/internal/apiclient"
)

// Client wraps apiclient.Client and maps its typed errors (StatusError /
// TransportError) onto brokerctl's stable exit codes. The degradation switch keys
// on HTTP 501 (reserved route, owning epic not landed), NOT 404.
type Client struct {
	api *apiclient.Client
}

// NewClient loads config, applies --port/--token/--config overrides, and builds a
// Client. Host comes from config (advisory; default 127.0.0.1) — E2 binds
// loopback-only so there is no host override flag.
func NewClient(opt GlobalOpts) (*Client, error) {
	cfg, err := LoadConfig(opt.ConfigPath)
	if err != nil {
		return nil, err
	}
	host := cfg.Host
	port := cfg.Port
	token := cfg.Token
	if opt.Port != 0 {
		port = opt.Port
	}
	if opt.Token != "" {
		token = opt.Token
	}
	hc := &http.Client{Timeout: opt.Timeout}
	return &Client{api: apiclient.New(host, port, token, hc)}, nil
}

// mapErr converts an apiclient transport/status error into a coded cliError.
// Key contract: HTTP 501 (reserved route, owning epic not landed yet) →
// ExitNotImplemented (6), a "feature not ready" signal the CLI degrades on; HTTP
// 404 (unknown route/resource, e.g. an unknown build id) → ExitBroker (5), a real
// error. The distinction matters for scripting and is the whole reason the CLI
// keys degradation on 501 and never on 404. 401→ExitAuth, every other ≥400→
// ExitBroker, transport failure→ExitUnavailable. nil passes through.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var te *apiclient.TransportError
	if errors.As(err, &te) {
		return wrap(ExitUnavailable, "%s", te.Error())
	}
	var se *apiclient.StatusError
	if errors.As(err, &se) {
		switch {
		case se.Status == http.StatusUnauthorized:
			return wrap(ExitAuth, "broker rejected token (401) — check the token in your config")
		case se.Status == http.StatusNotImplemented:
			return wrap(ExitNotImplemented,
				"%s %s: not available on this broker yet (%s)", se.Method, se.Path, se.Detail())
		case se.Status == http.StatusNotFound:
			return wrap(ExitBroker, "%s %s: not found (404: %s)", se.Method, se.Path, se.Detail())
		default:
			return wrap(ExitBroker, "%s", se.Error())
		}
	}
	return wrap(ExitBroker, "%s", err.Error())
}

// --- typed methods (delegate to apiclient, map errors) ---

func (c *Client) Healthz(ctx context.Context) (api.HealthResponse, error) {
	h, err := c.api.Healthz(ctx)
	return h, mapErr(err)
}

func (c *Client) ListBuildsResponse(ctx context.Context) (api.BuildsResponse, error) {
	r, err := c.api.ListBuilds(ctx)
	return r, mapErr(err)
}

func (c *Client) GetBuild(ctx context.Context, id string) (api.Build, error) {
	b, err := c.api.GetBuild(ctx, id)
	return b, mapErr(err)
}

func (c *Client) Kill(ctx context.Context, id string) (api.KillResult, error) {
	res, err := c.api.Kill(ctx, id)
	return res, mapErr(err)
}

func (c *Client) Drain(ctx context.Context) error  { return mapErr(c.api.Drain(ctx)) }
func (c *Client) Pause(ctx context.Context) error  { return mapErr(c.api.Pause(ctx)) }
func (c *Client) Resume(ctx context.Context) error { return mapErr(c.api.Resume(ctx)) }

func (c *Client) Profile(ctx context.Context, id string) (api.ProfileRef, error) {
	p, err := c.api.Profile(ctx, id)
	return p, mapErr(err)
}

// StreamEvents dials WS /events. A 401 on the upgrade maps to ExitAuth (no retry);
// any other dial failure maps to ExitUnavailable.
func (c *Client) StreamEvents(ctx context.Context) (*apiclient.EventStream, error) {
	s, err := c.api.StreamEvents(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return s, nil
}
