package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	// maxMessageSize caps one inbound message (one line on stdio, one event
	// or body on HTTP).
	maxMessageSize = 64 << 20
	// stderrCapacity is how much of a server's stderr is retained.
	stderrCapacity = 16 << 10
	// defaultShutdownGrace is how long a server gets after its stdin closes,
	// and again after SIGTERM, before it is killed.
	defaultShutdownGrace = 2 * time.Second
)

// stdioTransport runs a server as a child process and exchanges
// newline-delimited JSON-RPC messages over its stdin and stdout.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	stderr  *ringBuffer
	handler func([]byte)
	logf    func(string, ...any)
	grace   time.Duration

	// writeSem serialises writes; a channel so waiting respects contexts.
	writeSem chan struct{}

	readDone chan struct{}
	waitDone chan struct{}
	doneCh   chan struct{}

	mu       sync.Mutex
	waitErr  error
	finalErr error
	closing  bool

	closeOnce sync.Once
}

func newStdioTransport(cfg ServerConfig, dir string, grace time.Duration, handler func([]byte), logf func(string, ...any)) *stdioTransport {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = mergeEnv(os.Environ(), cfg.Env)
	cmd.Dir = dir
	setProcessGroup(cmd)
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	// WaitDelay bounds how long Wait blocks on stderr copying after the
	// process exits, in case a grandchild inherited the pipe.
	cmd.WaitDelay = grace
	return &stdioTransport{
		cmd:      cmd,
		stderr:   newRingBuffer(stderrCapacity),
		handler:  handler,
		logf:     logf,
		grace:    grace,
		writeSem: make(chan struct{}, 1),
		readDone: make(chan struct{}),
		waitDone: make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (t *stdioTransport) start() error {
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	// stdout is an os.Pipe we own, rather than cmd.StdoutPipe, so Wait never
	// closes it underneath the reader and the last messages are not lost.
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	t.cmd.Stdout = pw
	t.cmd.Stderr = t.stderr
	if err := t.cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = pr.Close()
		_ = pw.Close()
		return fmt.Errorf("start %s: %w", t.cmd.Path, err)
	}
	_ = pw.Close()
	t.stdin = stdin
	t.stdout = pr
	go t.readLoop()
	go func() {
		err := t.cmd.Wait()
		t.mu.Lock()
		t.waitErr = err
		t.mu.Unlock()
		close(t.waitDone)
	}()
	go t.supervise()
	return nil
}

func (t *stdioTransport) readLoop() {
	defer close(t.readDone)
	r := bufio.NewReaderSize(t.stdout, 64<<10)
	for {
		line, err := readLine(r, maxMessageSize)
		if errors.Is(err, errLineTooLong) {
			t.logf("dropped a message larger than %d bytes from the server", maxMessageSize)
			continue
		}
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' && line[0] != '[' {
			// Servers must keep stdout for protocol messages; keep stray
			// output with stderr so it still helps explain failures.
			fmt.Fprintf(t.stderr, "[stdout] %s\n", clip(string(line), 500))
			continue
		}
		t.handler(line)
	}
}

// supervise waits for the server to stop talking or exit, makes sure both
// have happened, records why and closes doneCh.
func (t *stdioTransport) supervise() {
	select {
	case <-t.readDone:
		// stdout closed; the process should exit on its own.
		select {
		case <-t.waitDone:
		case <-time.After(t.grace):
			kill(t.cmd)
			<-t.waitDone
		}
	case <-t.waitDone:
		// The process exited; drain what it already wrote. A grandchild
		// holding stdout open must not keep the transport alive.
		select {
		case <-t.readDone:
		case <-time.After(t.grace):
			_ = t.stdout.Close()
			<-t.readDone
		}
	}
	_ = t.stdout.Close()

	t.mu.Lock()
	if t.closing {
		t.finalErr = ErrClosed
	} else {
		t.finalErr = t.exitError()
	}
	t.mu.Unlock()
	close(t.doneCh)
}

// exitError describes an unexpected exit. Callers hold t.mu.
func (t *stdioTransport) exitError() error {
	reason := "server closed its output"
	if t.waitErr != nil {
		reason = "server exited: " + t.waitErr.Error()
	} else if t.cmd.ProcessState != nil {
		reason = "server exited with status 0"
	}
	if tail := t.stderr.Tail(10); tail != "" {
		return fmt.Errorf("mcp: %s; stderr:\n%s", reason, tail)
	}
	return errors.New("mcp: " + reason)
}

func (t *stdioTransport) send(ctx context.Context, msg []byte) error {
	select {
	case t.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.doneCh:
		return t.err()
	}
	defer func() { <-t.writeSem }()
	select {
	case <-t.doneCh:
		return t.err()
	default:
	}

	line := make([]byte, 0, len(msg)+1)
	line = append(append(line, msg...), '\n')
	written := make(chan error, 1)
	go func() {
		_, err := t.stdin.Write(line)
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			return fmt.Errorf("mcp: write to server: %w", err)
		}
		return nil
	case <-ctx.Done():
		// A partial line would corrupt the stream for every later message,
		// so a server that stopped reading its input is shut down.
		t.logf("server stopped reading its input; shutting it down")
		kill(t.cmd)
		<-written
		return ctx.Err()
	case <-t.doneCh:
		return t.err()
	}
}

// close ends the session as the MCP spec describes: close stdin, wait for
// the process to exit, then SIGTERM, then SIGKILL.
func (t *stdioTransport) close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		t.mu.Unlock()
		if t.stdin == nil {
			close(t.doneCh)
			return
		}
		_ = t.stdin.Close()
		select {
		case <-t.waitDone:
		case <-time.After(t.grace):
			terminate(t.cmd)
			select {
			case <-t.waitDone:
			case <-time.After(t.grace):
				kill(t.cmd)
				<-t.waitDone
			}
		}
		<-t.doneCh
	})
	return nil
}

func (t *stdioTransport) done() <-chan struct{} { return t.doneCh }

func (t *stdioTransport) err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finalErr
}

// stderrTail returns the last lines the server wrote to stderr.
func (t *stdioTransport) stderrTail(n int) string { return t.stderr.Tail(n) }
