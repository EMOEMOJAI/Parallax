package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// Query the resolver directly: net.Resolver may satisfy LookupNetIP from
// /etc/hosts, even with a custom Dial, which cannot measure DNS latency.
// A and AAAA share the caller's deadline and have at most two active sockets.
func queryDNSResolver(ctx context.Context, resolver benchResolver, fqdn string) ([]string, error) {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, err
	}
	type result struct {
		answers []string
		err     error
	}
	results := make(chan result, 2)
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		go func(typ dnsmessage.Type) {
			answers, err := queryDNSType(ctx, resolver, dnsmessage.Question{Name: name, Type: typ, Class: dnsmessage.ClassINET})
			results <- result{answers, err}
		}(typ)
	}
	var answers []string
	var firstErr error
	for i := 0; i < 2; i++ {
		r := <-results
		answers = append(answers, r.answers...)
		if firstErr == nil {
			firstErr = r.err
		}
	}
	if len(answers) > 0 {
		return answers, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, errors.New("DNS response contained no A or AAAA records")
}

func queryDNSType(ctx context.Context, resolver benchResolver, question dnsmessage.Question) ([]string, error) {
	var randomID [2]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(randomID[:])
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: true}, Questions: []dnsmessage.Question{question}}
	packet, err := query.Pack()
	if err != nil {
		return nil, err
	}
	for _, network := range []string{"udp", "tcp"} {
		response, err := exchangeDNS(ctx, resolver, network, packet, id, question)
		if err != nil {
			return nil, err
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(response); err != nil {
			return nil, fmt.Errorf("invalid DNS response: %w", err)
		}
		if msg.Truncated {
			if network == "udp" {
				continue
			}
			return nil, errors.New("truncated DNS response over TCP")
		}
		if msg.RCode != dnsmessage.RCodeSuccess {
			return nil, fmt.Errorf("DNS response: %s", msg.RCode)
		}
		return dnsAnswerAddresses(msg, question), nil
	}
	return nil, errors.New("truncated DNS response")
}

func exchangeDNS(ctx context.Context, resolver benchResolver, network string, packet []byte, id uint16, question dnsmessage.Question) ([]byte, error) {
	conn, err := dnsbenchResolverDialer(resolver)(ctx, network, "")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if network == "tcp" {
		framed := make([]byte, 2+len(packet))
		binary.BigEndian.PutUint16(framed, uint16(len(packet)))
		copy(framed[2:], packet)
		if _, err := io.Copy(conn, bytes.NewReader(framed)); err != nil {
			return nil, err
		}
	} else if _, err := conn.Write(packet); err != nil {
		return nil, err
	}
	// DNS messages are bounded by the protocol's 16-bit TCP length. The parser
	// validates compression pointers and section lengths inside that same cap.
	buffer := make([]byte, 65535)
	for {
		var response []byte
		if network == "tcp" {
			var size [2]byte
			if _, err := io.ReadFull(conn, size[:]); err != nil {
				return nil, err
			}
			n := int(binary.BigEndian.Uint16(size[:]))
			response = buffer[:n]
			if _, err := io.ReadFull(conn, response); err != nil {
				return nil, err
			}
		} else {
			n, err := conn.Read(buffer)
			if err != nil {
				return nil, err
			}
			response = buffer[:n]
		}
		var parser dnsmessage.Parser
		header, parseErr := parser.Start(response)
		echoed, questionErr := parser.Question()
		_, extraErr := parser.Question()
		valid := parseErr == nil && header.ID == id && header.Response && header.OpCode == 0 && questionErr == nil && extraErr == dnsmessage.ErrSectionDone && echoed.Type == question.Type && echoed.Class == question.Class && strings.EqualFold(echoed.Name.String(), question.Name.String())
		if !valid {
			if network == "udp" {
				// Ignore unrelated/forged datagrams until the deadline.
				continue
			}
			return nil, errors.New("DNS response does not match the question")
		}
		if header.Truncated && network == "udp" {
			// A truncated packet need not have parseable answer sections. Return a
			// minimal validated message so the caller retries the same query over TCP.
			return (&dnsmessage.Message{Header: header, Questions: []dnsmessage.Question{echoed}}).Pack()
		}
		return response, nil
	}
}

func dnsAnswerAddresses(msg dnsmessage.Message, question dnsmessage.Question) []string {
	// Accept only records for the queried name or its CNAME chain, never an
	// unrelated address from the answer/additional sections.
	name := strings.ToLower(question.Name.String())
	allowed := map[string]bool{name: true}
	for hop := 0; hop < 16; hop++ {
		changed := false
		for _, rr := range msg.Answers {
			cname, ok := rr.Body.(*dnsmessage.CNAMEResource)
			if !ok || rr.Header.Class != dnsmessage.ClassINET || !allowed[strings.ToLower(rr.Header.Name.String())] {
				continue
			}
			target := strings.ToLower(cname.CNAME.String())
			if !allowed[target] {
				allowed[target] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	var answers []string
	seen := map[string]bool{}
	for _, rr := range msg.Answers {
		if rr.Header.Class != dnsmessage.ClassINET || rr.Header.Type != question.Type || !allowed[strings.ToLower(rr.Header.Name.String())] {
			continue
		}
		var addr netip.Addr
		switch body := rr.Body.(type) {
		case *dnsmessage.AResource:
			addr = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			addr = netip.AddrFrom16(body.AAAA)
		default:
			continue
		}
		value := addr.Unmap().String()
		if !seen[value] {
			seen[value] = true
			answers = append(answers, value)
		}
	}
	return answers
}
