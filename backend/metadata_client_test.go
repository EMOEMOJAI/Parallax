package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestMetadataDialRejectsPrivateAnswersBeforeConnecting(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::", "fe80::1%eth0"} {
		t.Run(address, func(t *testing.T) {
			d := metadataDialer{
				lookup: func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr(address)}, nil
				},
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("mixed DNS answers must fail before any connection")
					return nil, nil
				},
			}
			if _, err := d.dialContext(context.Background(), "tcp", "metadata.example.com:443"); err == nil {
				t.Fatal("private answer accepted")
			}
		})
	}
}

func TestMetadataDialPinsValidatedAddress(t *testing.T) {
	lookups := 0
	sentinel := errors.New("synthetic connection result")
	d := metadataDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			lookups++
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		},
		dial: func(_ context.Context, _, target string) (net.Conn, error) {
			if target != "1.1.1.1:443" {
				t.Fatalf("unvalidated target: %s", target)
			}
			return nil, sentinel
		},
	}
	_, err := d.dialContext(context.Background(), "tcp", "metadata.example.com:443")
	if !errors.Is(err, sentinel) || lookups != 1 {
		t.Fatalf("result=%v lookups=%d", err, lookups)
	}
}

type metadataRoundTripper func(*http.Request) (*http.Response, error)

func (f metadataRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetadataRedirectCannotReachPrivateListener(t *testing.T) {
	private := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("metadata redirect reached a private listener")
	}))
	defer private.Close()
	client := newMetadataClient()
	transport := client.Transport
	client.Transport = metadataRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "rdap.org" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {private.URL}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
	if _, err := client.Get("https://rdap.org/domain/example.com"); err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("redirect result: %v", err)
	}
}

func TestMetadataRedirectPolicy(t *testing.T) {
	client := newMetadataClient()
	for _, target := range []string{"http://registry.example.com"} {
		r, _ := http.NewRequest("GET", target, nil)
		if client.CheckRedirect(r, nil) == nil {
			t.Fatal("unsafe redirect accepted")
		}
	}
	r, _ := http.NewRequest("GET", "https://registry.example.com", nil)
	r.URL.User = url.UserPassword("test-user", "test-secret")
	if client.CheckRedirect(r, nil) == nil {
		t.Fatal("credentialed redirect accepted")
	}
	r.URL.User = nil
	if client.CheckRedirect(r, []*http.Request{r}) != nil {
		t.Fatal("HTTPS registry redirect refused")
	}
	if client.CheckRedirect(r, make([]*http.Request, 5)) == nil {
		t.Fatal("redirect limit ignored")
	}
}

func TestMetadataHandlersPropagateCancellation(t *testing.T) {
	for _, path := range []string{"/api/geoip/1.1.1.1", "/api/rdap/example.com"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("CLIENT_API_KEY", "")
			s := NewServer()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			called := false
			s.geoClient = &http.Client{Transport: metadataRoundTripper(func(r *http.Request) (*http.Response, error) {
				called = true
				if !errors.Is(r.Context().Err(), context.Canceled) {
					t.Error("request cancellation lost")
				}
				return nil, context.Canceled
			})}
			r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
			w := httptest.NewRecorder()
			if strings.Contains(path, "geoip") {
				s.handleGeoIP(w, r)
			} else {
				s.handleRDAP(w, r)
			}
			if !called || w.Code != 502 {
				t.Fatalf("called=%v status=%d", called, w.Code)
			}
		})
	}
}
