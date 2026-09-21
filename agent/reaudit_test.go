package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/dns/dnsmessage"
)

func TestShellUTF8SurvivesPTYAndJSONChunkBoundaries(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	stopped := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var wire strings.Builder
	go func() {
		defer close(stopped)
		startShellSession("utf8-regression", func(id, typ, data string) {
			if typ != "shell_output" {
				return
			}
			b, _ := json.Marshal(CommandResponse{id, typ, data})
			var decoded CommandResponse
			json.Unmarshal(b, &decoded)
			mu.Lock()
			wire.WriteString(decoded.Data)
			mu.Unlock()
			select {
			case started <- struct{}{}:
			default:
			}
		}, done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not start")
	}
	handleShellInput("utf8-regression", ShellInput{Action: "data", Data: "printf '\\344'; sleep 0.1; printf '\\270\\255\\n'; exit\n"})
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(wire.String(), "中") {
		t.Fatalf("PTY character corrupted: %q", wire.String())
	}
}

func TestShellTextStreamHandlesEveryUTF8SplitAndEOF(t *testing.T) {
	text := "中文 🌐 café"
	for split := 0; split <= len(text); split++ {
		var stream shellTextStream
		result := stream.append([]byte(text[:split]), false) + stream.append([]byte(text[split:]), true)
		if result != text {
			t.Fatalf("split %d: %q", split, result)
		}
	}
	var stream shellTextStream
	if got := stream.append([]byte{0xe4}, false); got != "" {
		t.Fatalf("incomplete rune emitted: %q", got)
	}
	if got := stream.append(nil, true); got != "�" {
		t.Fatalf("EOF failed to flush invalid rune: %q", got)
	}
}

func TestShellStartUsesRequestedInitialSize(t *testing.T) {
	old := allowShell
	allowShell = true
	defer func() { allowShell = old }()
	s5RunAgainstServer(t, func(conn *websocket.Conn) {
		s5ReadEnvelope(conn)
		payload, _ := json.Marshal(map[string]any{"id": "initial-size", "cols": 88, "rows": 42})
		conn.WriteJSON(AgentMessage{Action: "shell_start", Payload: payload})
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var output strings.Builder
		sent := false
		for {
			env, _, err := s5ReadEnvelope(conn)
			if err != nil {
				t.Errorf("shell read: %v", err)
				return
			}
			var frame CommandResponse
			if json.Unmarshal(env.Payload, &frame) != nil {
				continue
			}
			if frame.Type == "shell_output" {
				output.WriteString(frame.Data)
				if !sent {
					sent = true
					payload, _ = json.Marshal(map[string]any{"id": "initial-size", "input": ShellInput{Action: "data", Data: "stty size; exit\n"}})
					conn.WriteJSON(AgentMessage{Action: "shell_input", Payload: payload})
				}
			}
			if frame.Type == "done" {
				break
			}
		}
		if !strings.Contains(output.String(), "42 88") {
			t.Errorf("first shell size was not supplied: %q", output.String())
		}
	})
	for _, tc := range []struct{ cols, rows, wantCols, wantRows uint16 }{{0, 0, 120, 40}, {600, 300, 500, 200}, {0, 20, 120, 20}} {
		size := initialShellSize(tc.cols, tc.rows)
		if size.Cols != tc.wantCols || size.Rows != tc.wantRows {
			t.Errorf("size %+v for %+v", size, tc)
		}
	}
}

func TestInvalidServerURLDoesNotLeakLegacyCredential(t *testing.T) {
	old := serverURL
	defer func() { serverURL = old }()
	serverURL = "ws://localhost:invalid/ws/agent?key=not-for-logs"
	err := run()
	if err == nil || strings.Contains(err.Error(), "not-for-logs") {
		t.Fatalf("unsafe dial error: %v", err)
	}
}

func TestDNSBenchQueriesWireForHostsFileNames(t *testing.T) {
	// localhost is always resolved locally by LookupNetIP. A real DNS query
	// must instead reach this private test resolver and use its wire answer.
	server := s7DNSResponder(t)
	answers, _, err := dnsbenchQuery(context.Background(), benchResolver{addr: server}, "localhost.")
	if err != nil || len(answers) != 1 || answers[0] != "93.184.216.34" {
		t.Fatalf("hosts-file lookup bypassed DNS: %v %v", answers, err)
	}
	// In the Linux runtime smoke this is a valid public-shaped FQDN installed
	// using --add-host; it must not yield that local entry's address either.
	if name := os.Getenv("PARALLAX_TEST_HOSTS_NAME"); name != "" {
		answers, _, err = dnsbenchQuery(context.Background(), benchResolver{addr: server}, name)
		if err != nil || len(answers) != 1 || answers[0] != "93.184.216.34" {
			t.Fatalf("FQDN hosts bypass: %v %v", answers, err)
		}
	}
}

func TestDNSBenchTruncatedUDPUsesTCPAndChecksTransaction(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	// Each A/AAAA UDP transaction first sees an unrelated packet, then a
	// matching truncated response. Both must retry over TCP to succeed.
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, peer, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buffer[:n]) != nil {
				continue
			}
			q.Header.Response = true
			q.Header.Truncated = true
			q.Header.ID++
			wrong, _ := q.Pack()
			udp.WriteTo(wrong, peer)
			q.Header.ID--
			right, _ := q.Pack()
			udp.WriteTo(right, peer)
		}
	}()
	var tcpCount int
	var mu sync.Mutex
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				q := make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := io.ReadFull(conn, q); err != nil {
					return
				}
				response, ok := s7DNSAnswer(q)
				if !ok {
					return
				}
				mu.Lock()
				tcpCount++
				mu.Unlock()
				binary.BigEndian.PutUint16(size[:], uint16(len(response)))
				conn.Write(size[:])
				conn.Write(response)
			}()
		}
	}()
	addr := netip.MustParseAddrPort(tcp.Addr().String())
	answers, _, err := dnsbenchQuery(context.Background(), benchResolver{addr: addr}, "example.com.")
	if err != nil || len(answers) != 1 || answers[0] != "93.184.216.34" {
		t.Fatalf("TCP fallback failed: %v %v", answers, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tcpCount != 2 {
		t.Fatalf("got %d TCP queries, want A and AAAA", tcpCount)
	}
}

func TestDNSBenchCancellationClosesSilentResolver(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := dnsbenchQuery(ctx, benchResolver{addr: netip.MustParseAddrPort(listener.LocalAddr().String())}, "example.com.")
		done <- err
	}()
	var buffer [512]byte
	listener.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := listener.ReadFrom(buffer[:]); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled lookup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("DNS cancellation left worker blocked")
	}
}

func TestDNSBenchRejectsUnrelatedAnswers(t *testing.T) {
	name := func(s string) dnsmessage.Name {
		n, err := dnsmessage.NewName(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	question := dnsmessage.Question{Name: name("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{Answers: []dnsmessage.Resource{
		{Header: dnsmessage.ResourceHeader{Name: name("unrelated.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}}},
		{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: name("alias.example.")}},
		{Header: dnsmessage.ResourceHeader{Name: name("alias.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: [4]byte{8, 8, 8, 8}}},
	}}
	got := dnsAnswerAddresses(msg, question)
	if len(got) != 1 || got[0] != "8.8.8.8" {
		t.Fatalf("unrelated answer included: %v", got)
	}
}

func TestDNSBenchCancellationAfterLastRowCannotReportSuccess(t *testing.T) {
	resolver := benchResolver{addr: s7DNSResponder(t), source: "system"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emit := &nativeEmitter{send: func(typ, data string) error {
		if typ == "output" && strings.Contains(data, "answer(s)") {
			cancel()
		}
		return nil
	}}
	err := probeDNSBenchResolvers(ctx, CommandRequest{Target: "example.com"}, emit, []benchResolver{resolver})
	if !errors.Is(err, context.Canceled) || emit.summary != nil {
		t.Fatalf("cancellation result: err=%v summary=%v", err, emit.summary)
	}
}
