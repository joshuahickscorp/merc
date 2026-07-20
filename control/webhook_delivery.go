package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	webhookRequestTimeout = 10 * time.Second
	webhookDialTimeout    = 5 * time.Second
	webhookURLMaxBytes    = 2048
	webhookMaxRedirects   = 0 // redirects are refused; the registered origin is the only data recipient
)

var errWebhookRedirectRefused = errors.New("webhook redirects are refused")

var webhookNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // current network / software aliases
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT shared space
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmark networks
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64 translation
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001:2::/48"),     // benchmark networks
	netip.MustParsePrefix("2001:10::/28"),    // reserved ORCHID range
	netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

type webhookIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type webhookTargetPolicy struct {
	resolver     webhookIPResolver
	allowPrivate bool
	allowHTTP    bool
	tlsConfig    *tls.Config // test-only custom roots/hooks; production leaves this nil
}

type permanentWebhookDeliveryError struct{ err error }

func (e *permanentWebhookDeliveryError) Error() string { return e.err.Error() }
func (e *permanentWebhookDeliveryError) Unwrap() error { return e.err }

func permanentWebhookFailure(err error) error {
	if err == nil {
		return nil
	}
	var permanent *permanentWebhookDeliveryError
	if errors.As(err, &permanent) {
		return err
	}
	return &permanentWebhookDeliveryError{err: err}
}

func webhookFailureIsPermanent(err error) bool {
	var permanent *permanentWebhookDeliveryError
	return errors.As(err, &permanent)
}

func allowPrivateWebhookHosts() bool {
	return allowPrivateWebhookHostsForProcess(testing.Testing())
}

func allowPrivateWebhookHostsForProcess(isTest bool) bool { return isTest }

func validateWebhookURLSyntax(raw string, allowHTTP bool) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > webhookURLMaxBytes {
		return nil, fmt.Errorf("webhook url must be between 1 and %d bytes", webhookURLMaxBytes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("webhook url parse: %w", err)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, fmt.Errorf("webhook url scheme %q not allowed", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("webhook url has no host")
	}
	if u.User != nil {
		return nil, errors.New("webhook url must not contain user info")
	}
	if strings.Contains(u.Hostname(), "%") {
		return nil, errors.New("webhook url must not contain an IPv6 zone")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return nil, fmt.Errorf("webhook url port %q is invalid", port)
		}
	}
	return u, nil
}

type resolvedWebhookTarget struct {
	host string
	port string
	ips  []net.IP
}

func resolveWebhookTarget(ctx context.Context, raw string, policy webhookTargetPolicy) (resolvedWebhookTarget, error) {
	u, err := validateWebhookURLSyntax(raw, policy.allowHTTP)
	if err != nil {
		return resolvedWebhookTarget{}, err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	resolver := policy.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	resolved, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return resolvedWebhookTarget{}, fmt.Errorf("webhook host resolve %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return resolvedWebhookTarget{}, fmt.Errorf("webhook host %q resolved to no addresses", host)
	}
	ips := make([]net.IP, 0, len(resolved))
	seen := make(map[string]bool, len(resolved))
	for _, candidate := range resolved {
		ip := candidate.IP
		if ip == nil {
			continue
		}
		if !policy.allowPrivate && isInternalWebhookIP(ip) {
			return resolvedWebhookTarget{}, fmt.Errorf(
				"webhook host %q resolves to non-public address %s (refused: SSRF guard)", host, ip)
		}
		key := ip.String()
		if !seen[key] {
			seen[key] = true
			ips = append(ips, append(net.IP(nil), ip...))
		}
	}
	if len(ips) == 0 {
		return resolvedWebhookTarget{}, fmt.Errorf("webhook host %q resolved to no usable addresses", host)
	}
	return resolvedWebhookTarget{host: host, port: port, ips: ips}, nil
}

type webhookPinnedTransport struct {
	policy webhookTargetPolicy
}

func (t *webhookPinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := resolveWebhookTarget(req.Context(), req.URL.String(), t.policy)
	if err != nil {
		return nil, permanentWebhookFailure(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // environment proxies would bypass the validated destination
	transport.DisableKeepAlives = true
	transport.MaxConnsPerHost = 1
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.ResponseHeaderTimeout = webhookDialTimeout
	transport.TLSHandshakeTimeout = webhookDialTimeout
	if t.policy.tlsConfig != nil {
		transport.TLSClientConfig = t.policy.tlsConfig.Clone()
	} else {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	dialer := &net.Dialer{Timeout: webhookDialTimeout, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var errs []error
		for _, ip := range target.ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), target.port))
			if dialErr == nil {
				return conn, nil
			}
			errs = append(errs, dialErr)
		}
		return nil, fmt.Errorf("dialing validated webhook target %s: %w", target.host, errors.Join(errs...))
	}
	return transport.RoundTrip(req)
}

func newWebhookHTTPClient() *http.Client {
	policy := webhookTargetPolicy{
		resolver:     net.DefaultResolver,
		allowPrivate: allowPrivateWebhookHosts(),
		allowHTTP:    testing.Testing(),
	}
	return newWebhookHTTPClientWithPolicy(policy)
}

func newWebhookHTTPClientWithPolicy(policy webhookTargetPolicy) *http.Client {
	return &http.Client{
		Timeout:   webhookRequestTimeout,
		Transport: &webhookPinnedTransport{policy: policy},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > webhookMaxRedirects {
				return permanentWebhookFailure(errWebhookRedirectRefused)
			}
			return nil
		},
	}
}

func isInternalWebhookIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range webhookNonPublicPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func webhookHTTPStatusIsRetryable(code int) bool {
	if code >= 500 {
		return true
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func webhookRetryBackoff(failedAttempts int) time.Duration {
	if failedAttempts < 1 {
		failedAttempts = 1
	}
	const base = 30 * time.Second
	const max = 6 * time.Hour
	shift := failedAttempts - 1
	if shift > 16 {
		shift = 16
	}
	d := base * time.Duration(1<<shift)
	if d > max {
		return max
	}
	return d
}
