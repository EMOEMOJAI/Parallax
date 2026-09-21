package main

import (
	"encoding/json"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func FuzzCommandSummary(f *testing.F) {
	for _, seed := range []string{"", "1 packets transmitted, 1 received, 0% packet loss", "rtt min/avg/max/mdev = 1/2/3/0 ms", "HTTP/1.1 200 OK", ";; Query time: 2 msec", "\xff\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64*1024 {
			t.Skip()
		}
		tail := newTailBuffer()
		for _, line := range strings.Split(input, "\n") {
			tail.add(line)
		}
		for _, kind := range []string{"ping", "http", "dns", "mtr"} {
			result := summaryJSON(kind, tail.tail())
			if len(result) > 0 && !json.Valid(result) {
				t.Fatalf("%s emitted invalid JSON", kind)
			}
		}
	})
}

func FuzzProbeTargets(f *testing.F) {
	for _, seed := range []string{"example.com:443", "[2001:db8::1]:443", "127.0.0.1:0", "host:65536", "host:1,2", "::ffff:127.0.0.1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			t.Skip()
		}
		host, port, err := splitSingleHostPort(input)
		if err == nil {
			n, e := strconv.Atoi(port)
			if e != nil || n < 1 || n > 65535 || strings.TrimSpace(host) == "" {
				t.Fatal("accepted invalid host/port")
			}
			h, p, e := net.SplitHostPort(net.JoinHostPort(host, port))
			if e != nil || h != host || p != port {
				t.Fatal("target cannot round-trip")
			}
		}
		if addr, err := netip.ParseAddr(input); err == nil {
			if isBlockedAddr(addr) != isBlockedAddr(addr.Unmap().WithZone("")) {
				t.Fatal("zone or mapped encoding changes address policy")
			}
			if (addr.IsUnspecified() || addr.IsMulticast()) && !isBlockedAddr(addr) {
				t.Fatal("non-unicast target accepted")
			}
		}
	})
}

func FuzzDNSAnswers(f *testing.F) {
	name := dnsmessage.MustNewName("example.com.")
	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		rr := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: name, Type: typ, Class: dnsmessage.ClassINET}}
		if typ == dnsmessage.TypeA {
			rr.Body = &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}}
		} else {
			rr.Body = &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::1").As16()}
		}
		msg := dnsmessage.Message{Header: dnsmessage.Header{Response: true}, Answers: []dnsmessage.Resource{rr}}
		seed, err := msg.Pack()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	f.Add([]byte{0, 0, 255})
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 65535 {
			t.Skip()
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(input); err != nil {
			return
		}
		for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			q := question
			q.Type = typ
			answers := dnsAnswerAddresses(msg, q)
			seen := map[string]bool{}
			if len(answers) > len(msg.Answers) {
				t.Fatal("more addresses than answer records")
			}
			for _, answer := range answers {
				if _, err := netip.ParseAddr(answer); err != nil || seen[answer] {
					t.Fatal("invalid or duplicate DNS address")
				}
				seen[answer] = true
			}
		}
	})
}
