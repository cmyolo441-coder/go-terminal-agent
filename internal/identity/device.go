package identity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Device flow errors.
var (
	ErrDeviceFlowUnsupported = errors.New("identity: provider has no device authorization endpoint")
	ErrDeviceCodeExpired     = errors.New("identity: device code expired before authorization")
	ErrAccessDenied          = errors.New("identity: authorization denied by user")
)

const (
	deviceGrantType       = "urn:ietf:params:oauth:grant-type:device_code"
	defaultDeviceInterval = 5 * time.Second
	slowDownIncrement     = 5 * time.Second
	maxPollInterval       = 60 * time.Second
	maxTransientFailures  = 5
)

// DeviceAuthorization is a device authorization response (RFC 8628 §3.2).
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval,omitempty"`
	// Google and some older providers use "verification_url".
	VerificationURL string `json:"verification_url,omitempty"`

	// ExpiresAt is computed from ExpiresIn when the response is received.
	ExpiresAt time.Time `json:"-"`
}

// String redacts the device code, which is a bearer secret until used.
func (d DeviceAuthorization) String() string {
	return fmt.Sprintf("DeviceAuthorization{user_code=%s uri=%s expires=%s}", d.UserCode, d.VerificationURI, d.ExpiresAt.Format(time.RFC3339))
}

// StartDeviceAuthorization begins the device flow (RFC 8628 §3.1). Show the
// user UserCode and VerificationURI (or VerificationURIComplete), then call
// PollDeviceToken.
func (c *Client) StartDeviceAuthorization(ctx context.Context, scopes ...string) (*DeviceAuthorization, error) {
	if c.meta.DeviceAuthorizationEndpoint == "" {
		return nil, ErrDeviceFlowUnsupported
	}
	if len(scopes) == 0 {
		scopes = c.scopes
	}
	form := url.Values{"scope": {strings.Join(scopes, " ")}}
	var da DeviceAuthorization
	if err := c.postForm(ctx, c.meta.DeviceAuthorizationEndpoint, form, &da); err != nil {
		return nil, fmt.Errorf("identity: device authorization: %w", err)
	}
	if da.VerificationURI == "" {
		da.VerificationURI = da.VerificationURL
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, errors.New("identity: device authorization response is missing required fields")
	}
	if da.ExpiresIn <= 0 {
		return nil, errors.New("identity: device authorization response has no expires_in")
	}
	// The user will open these URLs: refuse anything but https (loopback
	// http is allowed for local providers).
	if err := validateEndpointURL(da.VerificationURI); err != nil {
		return nil, fmt.Errorf("identity: verification_uri: %w", err)
	}
	if da.VerificationURIComplete != "" {
		if err := validateEndpointURL(da.VerificationURIComplete); err != nil {
			return nil, fmt.Errorf("identity: verification_uri_complete: %w", err)
		}
	}
	if da.Interval <= 0 {
		da.Interval = int64(defaultDeviceInterval / time.Second)
	}
	da.ExpiresAt = c.now().Add(time.Duration(da.ExpiresIn) * time.Second)
	return &da, nil
}

// PollDeviceToken polls the token endpoint until the user approves or
// denies the request or the device code expires (RFC 8628 §3.4-3.5). It
// honours "slow_down" by adding 5 seconds to the interval, and backs off
// exponentially on transient network or server errors.
func (c *Client) PollDeviceToken(ctx context.Context, da *DeviceAuthorization) (*Token, error) {
	if da == nil || da.DeviceCode == "" {
		return nil, errors.New("identity: PollDeviceToken: no device code")
	}
	interval := time.Duration(max(da.Interval, 1)) * time.Second
	failures := 0
	form := func() url.Values {
		return url.Values{"grant_type": {deviceGrantType}, "device_code": {da.DeviceCode}}
	}
	for {
		if !da.ExpiresAt.IsZero() && !c.now().Before(da.ExpiresAt) {
			return nil, ErrDeviceCodeExpired
		}
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		tok, err := c.tokenRequest(ctx, form(), "")
		if err == nil {
			return tok, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var oe *OAuthError
		if errors.As(err, &oe) {
			failures = 0
			switch oe.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval = min(interval+slowDownIncrement, maxPollInterval)
				continue
			case "access_denied":
				return nil, fmt.Errorf("%w: %s", ErrAccessDenied, oe.Description)
			case "expired_token":
				return nil, ErrDeviceCodeExpired
			default:
				return nil, err
			}
		}
		if !isTransient(err) {
			return nil, err
		}
		failures++
		if failures > maxTransientFailures {
			return nil, fmt.Errorf("identity: device token polling failed %d times: %w", failures, err)
		}
		interval = min(interval*2, maxPollInterval)
	}
}

// isTransient reports whether err is a network failure or a 5xx/429
// response worth retrying.
func isTransient(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 500 || he.StatusCode == 429
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// DeviceLogin runs the whole device flow: it starts authorization, calls
// show so the caller can display the code (and, if it wishes, open a
// browser), then polls until a token is issued.
func (c *Client) DeviceLogin(ctx context.Context, show func(*DeviceAuthorization) error, scopes ...string) (*Token, error) {
	da, err := c.StartDeviceAuthorization(ctx, scopes...)
	if err != nil {
		return nil, err
	}
	if show != nil {
		if err := show(da); err != nil {
			return nil, err
		}
	}
	return c.PollDeviceToken(ctx, da)
}
