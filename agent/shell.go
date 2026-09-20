package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/creack/pty"
)

// Per-shell output throttle: refill 1 MB/s, burst up to 4 MB. Bypassable scenarios
// (e.g. a paste-back of large content) are rare and benign; the goal is to stop
// `yes`-style infinite output from saturating the WebSocket.
const (
	shellRefillBytesPerSec = 1 * 1024 * 1024
	shellBurstBytes        = 4 * 1024 * 1024
)

type tokenBucket struct {
	mu       sync.Mutex
	tokens   int64
	max      int64
	refill   int64 // bytes per second
	lastFill time.Time
}

func newTokenBucket(burst, refill int64) *tokenBucket {
	return &tokenBucket{tokens: burst, max: burst, refill: refill, lastFill: time.Now()}
}

// take blocks until at least `n` bytes worth of tokens are available, then
// deducts them. n must be ≤ max (call sites split larger chunks).
func (b *tokenBucket) take(n int64) {
	if n <= 0 {
		return
	}
	if n > b.max {
		n = b.max
	}
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.lastFill).Seconds()
		if elapsed > 0 {
			b.tokens += int64(elapsed * float64(b.refill))
			if b.tokens > b.max {
				b.tokens = b.max
			}
			b.lastFill = now
		}
		if b.tokens >= n {
			b.tokens -= n
			b.mu.Unlock()
			return
		}
		// Sleep just long enough for the next token to refill.
		needed := n - b.tokens
		wait := time.Duration(float64(needed) / float64(b.refill) * float64(time.Second))
		b.mu.Unlock()
		if wait < 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}
		time.Sleep(wait)
	}
}

// ShellSession manages an interactive PTY shell
type ShellSession struct {
	id     string
	cmd    *exec.Cmd
	ptmx   *os.File
	input  chan ShellInput
	cancel context.CancelFunc
}

type ShellInput struct {
	Action string `json:"action"` // data, resize
	Data   string `json:"data,omitempty"`
	Cols   uint16 `json:"cols,omitempty"`
	Rows   uint16 `json:"rows,omitempty"`
}

var (
	shellSessions    = make(map[string]*ShellSession)
	shellSessionsMu  sync.RWMutex
	maxShellSessions = 5
)

// startShellSession spawns an interactive shell and streams output back.
// It blocks until the shell exits or the done channel is closed (agent disconnect).
func startShellSession(sessionID string, send func(string, string, string), done <-chan struct{}) {
	startShellSessionSize(sessionID, 0, 0, send, done)
}

func startShellSessionSize(sessionID string, cols, rows uint16, send func(string, string, string), done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, refusal := registerExecRun(sessionID, cancel)
	if refusal != "" {
		send(sessionID, "error", refusal)
		send(sessionID, "done", "")
		return
	}
	defer unregisterExecRun(sessionID, run)

	shellSessionsMu.Lock()
	if len(shellSessions) >= maxShellSessions {
		shellSessionsMu.Unlock()
		send(sessionID, "error", "Maximum concurrent shell sessions reached")
		send(sessionID, "done", "")
		return
	}
	// Reserve the slot under the write lock to prevent TOCTOU race
	shellSessions[sessionID] = nil
	shellSessionsMu.Unlock()
	defer func() {
		shellSessionsMu.Lock()
		delete(shellSessions, sessionID)
		shellSessionsMu.Unlock()
	}()

	// Cancel startup too if the connection closed before this goroutine ran.
	select {
	case <-done:
		cancel()
	default:
	}

	shell := "/bin/bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "/bin/sh"
	}

	cmd := exec.CommandContext(ctx, shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, initialShellSize(cols, rows))
	if err != nil {
		send(sessionID, "error", "Failed to start shell: "+err.Error())
		send(sessionID, "done", "")
		return
	}

	// pty returns blocking descriptors on some platforms. Register a
	// nonblocking duplicate with Go's poller so close and deadlines interrupt
	// pending reads/writes instead of stranding a shell worker.
	pollable, err := pollablePTY(ptmx)
	if err != nil {
		ptmx.Close()
		cancel()
		cmd.Wait()
		send(sessionID, "error", "Failed to configure shell I/O: "+err.Error())
		send(sessionID, "done", "")
		return
	}
	ptmx = pollable

	session := &ShellSession{
		id:     sessionID,
		cmd:    cmd,
		ptmx:   ptmx,
		input:  make(chan ShellInput, 16),
		cancel: cancel,
	}

	shellSessionsMu.Lock()
	shellSessions[sessionID] = session
	shellSessionsMu.Unlock()

	// Use sync.Once to prevent double-close race between defer and done-channel goroutine
	var cleanupOnce sync.Once
	doCleanup := func() {
		ptmx.Close()
		cancel()
	}

	defer func() {
		cleanupOnce.Do(doCleanup)
		cmd.Wait()
		send(sessionID, "done", "")
		log.Printf("Shell session %q ended", sessionID)
	}()

	log.Printf("Shell session %q started", sessionID)

	// Monitor the done channel to kill the shell on agent disconnect
	watcherDone := make(chan struct{})
	defer func() {
		cancel()
		<-watcherDone
	}()
	go func() {
		defer close(watcherDone)
		select {
		case <-done:
		case <-ctx.Done():
		}
		cleanupOnce.Do(doCleanup)
	}()

	inputDone := make(chan struct{})
	defer func() {
		cancel()
		cleanupOnce.Do(doCleanup)
		<-inputDone
	}()
	go func() {
		defer close(inputDone)
		for {
			select {
			case <-ctx.Done():
				return
			case input := <-session.input:
				if err := writeShellInput(session, input); err != nil {
					log.Printf("Shell %q input error: %v", sessionID, err)
					cancel()
					return
				}
			}
		}
	}()

	bucket := newTokenBucket(shellBurstBytes, shellRefillBytesPerSec)
	buf := make([]byte, 4096)
	var textStream shellTextStream
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			// Throttle before sending to bound the rate at which a hostile
			// shell can flood the WebSocket. take() blocks (briefly) when the
			// session has exceeded its burst budget.
			bucket.take(int64(n))
			if text := textStream.append(buf[:n], false); text != "" {
				send(sessionID, "shell_output", text)
			}
		}
		if err != nil {
			if text := textStream.append(nil, true); text != "" {
				send(sessionID, "shell_output", text)
			}
			return
		}
	}
}

// A PTY read can end inside a UTF-8 character. JSON strings must contain
// complete characters, so carry the at-most-three trailing bytes to the next
// read. Truly invalid bytes (including an incomplete final rune) are replaced.
type shellTextStream struct{ pending []byte }

func (s *shellTextStream) append(chunk []byte, final bool) string {
	s.pending = append(s.pending, chunk...)
	end := 0
	for end < len(s.pending) {
		if !final && !utf8.FullRune(s.pending[end:]) {
			break
		}
		_, size := utf8.DecodeRune(s.pending[end:])
		end += size
	}
	text := strings.ToValidUTF8(string(s.pending[:end]), "\uFFFD")
	s.pending = append(s.pending[:0], s.pending[end:]...)
	return text
}

func initialShellSize(cols, rows uint16) *pty.Winsize {
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 40
	}
	return &pty.Winsize{Cols: min(cols, 500), Rows: min(rows, 200)}
}

func handleShellInput(sessionID string, input ShellInput) {
	shellSessionsMu.RLock()
	session, ok := shellSessions[sessionID]
	shellSessionsMu.RUnlock()
	if !ok || session == nil {
		return
	}

	// Never let a shell that stopped consuming stdin block the WebSocket's
	// read loop (including heartbeat, cancellation and other commands).
	if len(input.Data) > 16384 {
		input.Data = input.Data[:16384]
	}
	select {
	case session.input <- input:
	default:
		session.cancel()
	}
}

func writeShellInput(session *ShellSession, input ShellInput) error {
	switch input.Action {
	case "data":
		if err := session.ptmx.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		_, err := session.ptmx.Write([]byte(input.Data))
		return err
	case "resize":
		if input.Cols == 0 || input.Rows == 0 {
			return nil
		}
		size := pty.Winsize{Cols: min(input.Cols, 500), Rows: min(input.Rows, 200)}
		// Use RawConn rather than pty.Setsize, whose Fd call switches a
		// pollable os.File back to blocking mode.
		raw, err := session.ptmx.SyscallConn()
		if err != nil {
			return err
		}
		var ioctlErr error
		err = raw.Control(func(fd uintptr) {
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
			if errno != 0 {
				ioctlErr = errno
			}
		})
		if err != nil {
			return err
		}
		return ioctlErr
	}
	return nil
}

func pollablePTY(original *os.File) (*os.File, error) {
	// Keep concurrent execs from inheriting the duplicate before CLOEXEC is set.
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(original.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), original.Name())
	original.Close()
	return file, nil
}
