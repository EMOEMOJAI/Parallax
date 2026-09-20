package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestZonedIPv6CannotBypassProbeAddressPolicy(t *testing.T) {
	s7Seams(t)
	storeLocalPrefixes([]netip.Prefix{netip.MustParsePrefix("2606:4700:1234::/64")})
	for _, tc := range []struct {
		name, address string
		relaxable     bool
	}{
		{"on-link", "2606:4700:1234::1%eth0", true},
		{"documentation", "2001:db8::1%eth0", false},
		{"nat64-well-known", "64:ff9b::a00:1%eth0", false},
		{"nat64-local-use", "64:ff9b:1::1%eth0", false},
		{"site-local", "fec0::1%eth0", true},
		{"link-local", "fe80::1%eth0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, allowPrivate := range []bool{false, true} {
				probeAllowPrivate.Store(allowPrivate)
				wantBlocked := !allowPrivate || !tc.relaxable
				addr := netip.MustParseAddr(tc.address)
				if got := isBlockedAddr(addr); got != wantBlocked {
					t.Errorf("allowPrivate=%v: blocked=%v, want %v", allowPrivate, got, wantBlocked)
				}
				// Exercise both the literal and resolver-answer admission paths.
				s7StubLookup(t, tc.address)
				for _, target := range []string{tc.address, "scoped.example"} {
					addrs, err := resolveAndCheck(context.Background(), target)
					if wantBlocked {
						if !errors.Is(err, errBlockedAddress) {
							t.Errorf("allowPrivate=%v target=%s: expected policy refusal, got %v", allowPrivate, target, err)
						}
					} else if err != nil || len(addrs) != 1 || addrs[0] != addr {
						t.Errorf("allowPrivate=%v target=%s: scoped literal must survive resolution: %v, %v", allowPrivate, target, addrs, err)
					}
				}
				// Calling Control directly checks final socket admission without
				// creating a socket or sending traffic to these reserved targets.
				err := probeDialControl("tcp6", net.JoinHostPort(tc.address, "443"), nil)
				if errors.Is(err, errBlockedAddress) != wantBlocked || (!wantBlocked && err != nil) {
					t.Errorf("allowPrivate=%v: final socket policy got %v, want blocked=%v", allowPrivate, err, wantBlocked)
				}
			}
		})
	}
}

func TestAddressPolicyHelpersCanonicalizeZones(t *testing.T) {
	s7Seams(t)
	storeLocalPrefixes([]netip.Prefix{netip.MustParsePrefix("2606:4700:1234::/64")})
	if !addrInLocalPrefix(netip.MustParseAddr("2606:4700:1234::1%eth0")) {
		t.Fatal("zoned on-link address was not classified as local")
	}
	if !isRelaxableAddr(netip.MustParseAddr("fec0::1%eth0")) {
		t.Fatal("zoned site-local address was not classified as private")
	}
}
