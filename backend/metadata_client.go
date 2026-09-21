package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Metadata providers and their redirects must never become a route into the
// server's private network. Resolve once, validate every answer, then dial the
// validated literal; a second DNS resolution would permit rebinding.
func metadataPublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}

type metadataDialer struct {
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func (d metadataDialer) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := d.lookup(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("metadata host has no addresses")
	}
	for _, ip := range ips {
		if !metadataPublicAddress(ip) {
			return nil, fmt.Errorf("metadata destination is not public")
		}
	}
	for _, ip := range ips {
		var conn net.Conn
		conn, err = d.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func newMetadataClient() *http.Client {
	d := metadataDialer{lookup: net.DefaultResolver.LookupNetIP, dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// An environment proxy resolves destinations itself and would bypass the
	// address check. These fixed public-provider lookups use direct transport.
	transport.Proxy = nil
	transport.DialContext = d.dialContext
	transport.MaxConnsPerHost = 16
	transport.ResponseHeaderTimeout = 5 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil {
				return fmt.Errorf("metadata redirect refused")
			}
			return nil
		},
	}
}
