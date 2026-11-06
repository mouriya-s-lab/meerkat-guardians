// Package omprpc drives one `omp --mode rpc` child process over its newline-delimited JSON protocol
// (protocol v1): it waits for the ready frame, correlates command responses by id and delivers every
// other stdout frame as an Event.
package omprpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	maxFrameBytes   = 64 << 20
	stderrTailBytes = 64 << 10
)

// Options describes the child process. Env is appended to os.Environ().
type Options struct {
	Bin  string
	Args []string
	Dir  string
	Env  []string
}

// Process is a running omp RPC child.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *ring

	writeMu sync.Mutex
	nextID  atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage

	events chan Event
	quit   chan struct{} // closed by Wait: undelivered events are dropped

	stdoutDone chan struct{} // closed when stdout reaches EOF
	exited     chan struct{} // closed when the process has exited
	waitErr    error
	waitOnce   sync.Once
}

// Start spawns the process and waits for its ready frame. The ctx bounds only that wait;
// the process outlives it. If the process exits first, the error carries the stderr tail.
func Start(ctx context.Context, o Options) (*Process, error) {
	cmd := exec.Command(o.Bin, o.Args...)
	cmd.Dir = o.Dir
	cmd.Env = append(os.Environ(), o.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("start %s: %w", o.Bin, err)
	}
	stdoutW.Close()
	stderrW.Close()

	p := &Process{
		cmd:        cmd,
		stdin:      stdin,
		stderr:     &ring{max: stderrTailBytes},
		pending:    map[string]chan json.RawMessage{},
		events:     make(chan Event),
		quit:       make(chan struct{}),
		stdoutDone: make(chan struct{}),
		exited:     make(chan struct{}),
	}
	pid := cmd.Process.Pid
	go p.copyStderr(stderrR, pid)
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()

	ready := make(chan struct{})
	raw := make(chan Event)
	go p.readStdout(stdoutR, ready, raw)
	go pump(raw, p.events, p.quit)

	select {
	case <-ready:
		return p, nil
	case <-p.stdoutDone:
		<-p.exited
		return nil, fmt.Errorf("omp exited before ready (%v): %s", p.waitErr, p.StderrTail())
	case <-ctx.Done():
		_ = p.Kill()
		<-p.exited
		close(p.quit)
		return nil, fmt.Errorf("omp not ready: %w: %s", ctx.Err(), p.StderrTail())
	}
}

// Events delivers every stdout frame except the ready frame and responses to Call, in order.
// It is closed when stdout ends, or when Wait returns.
func (p *Process) Events() <-chan Event { return p.events }

// Call assigns an id to cmd, writes it as one line and waits for the response with that id.
// A response with success:false is returned as an error naming the command and its message.
func (p *Process) Call(ctx context.Context, cmd map[string]any) (json.RawMessage, error) {
	id := "pm-" + strconv.FormatUint(p.nextID.Add(1), 10)
	frame := make(map[string]any, len(cmd)+1)
	for k, v := range cmd {
		frame[k] = v
	}
	frame["id"] = id
	line, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}

	ch := make(chan json.RawMessage, 1)
	p.pendingMu.Lock()
	p.pending[id] = ch
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}()

	p.writeMu.Lock()
	_, err = p.stdin.Write(append(line, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write %v: %w", cmd["type"], err)
	}

	select {
	case resp := <-ch:
		var r struct {
			Command string          `json:"command"`
			Success bool            `json:"success"`
			Error   string          `json:"error"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(resp, &r); err != nil {
			return nil, fmt.Errorf("decode %v response: %w", cmd["type"], err)
		}
		if !r.Success {
			return nil, fmt.Errorf("omp %s failed: %s", r.Command, r.Error)
		}
		return r.Data, nil
	case <-p.stdoutDone:
		return nil, fmt.Errorf("omp exited before responding to %v: %s", cmd["type"], p.StderrTail())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Prompt sends a prompt command and waits for its acknowledgement; the run's outcome arrives
// later as a prompt_result event.
func (p *Process) Prompt(ctx context.Context, message string) error {
	_, err := p.Call(ctx, map[string]any{"type": "prompt", "message": message})
	return err
}

// CloseStdin ends the command stream; omp drains accepted commands and exits.
func (p *Process) CloseStdin() error { return p.stdin.Close() }

// Wait waits for the process to exit and returns its exit error. Events not yet received are dropped.
func (p *Process) Wait() error {
	<-p.exited
	p.waitOnce.Do(func() { close(p.quit) })
	return p.waitErr
}

// Kill kills the process.
func (p *Process) Kill() error {
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// StderrTail returns the last 64 KiB written to stderr.
func (p *Process) StderrTail() string { return p.stderr.String() }

func (p *Process) readStdout(r *os.File, ready chan<- struct{}, out chan<- Event) {
	defer r.Close()
	defer close(p.stdoutDone)
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxFrameBytes)
	isReady := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			slog.Warn("omp: undecodable stdout frame", "pid", p.cmd.Process.Pid, "err", err)
			continue
		}
		frame := json.RawMessage(bytes.Clone(line))
		switch {
		case head.Type == "ready" && !isReady:
			isReady = true
			close(ready)
			continue
		case head.Type == "response" && head.ID != "":
			p.pendingMu.Lock()
			ch, ok := p.pending[head.ID]
			p.pendingMu.Unlock()
			if ok {
				ch <- frame
				continue
			}
		}
		out <- Event{Type: head.Type, Raw: frame}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("omp: stdout read failed", "pid", p.cmd.Process.Pid, "err", err)
	}
}

func (p *Process) copyStderr(r *os.File, pid int) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for sc.Scan() {
		line := sc.Bytes()
		p.stderr.Write(append(line, '\n'))
		slog.Debug("omp stderr", "pid", pid, "line", string(line))
	}
}

// pump forwards in to out through an unbounded queue, so the stdout reader never blocks on a
// consumer that is itself waiting for a Call response. It closes out when in is closed and the
// queue is drained, or when quit is closed.
func pump(in <-chan Event, out chan<- Event, quit <-chan struct{}) {
	defer close(out)
	var queue []Event
	for in != nil || len(queue) > 0 {
		var send chan<- Event
		var head Event
		if len(queue) > 0 {
			send = out
			head = queue[0]
		}
		select {
		case e, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			queue = append(queue, e)
		case send <- head:
			queue[0] = Event{}
			queue = queue[1:]
		case <-quit:
			// Keep draining the reader so it never blocks; drop what nobody will read.
			for range in {
			}
			return
		}
	}
}

// ring keeps the last max bytes written to it.
type ring struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (r *ring) Write(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, b...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
