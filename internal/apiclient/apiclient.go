// Package apiclient is the brokerctl -> broker HTTP client. E0 ships a typed
// stub (base URL + bearer token); E6 implements the real calls against E2's API.
package apiclient

import "net/http"

// Client talks to the broker over loopback HTTP with a bearer token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New constructs a client for the given base URL and bearer token.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: http.DefaultClient}
}
