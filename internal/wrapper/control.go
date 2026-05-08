package wrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"

	"snitch/internal/state"
)

type controlServer struct {
	socketPath string
	listener   net.Listener

	getState     func() *state.Session
	onSetAutoYes func(bool)
	onApproveNow func()

	subsMu sync.Mutex
	subs   []chan *state.Session
}

func newControlServer(socketPath string) (*controlServer, error) {
	_ = os.Remove(socketPath)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return &controlServer{socketPath: socketPath, listener: l}, nil
}

func (c *controlServer) serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		c.listener.Close()
	}()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go c.handle(conn)
	}
}

func (c *controlServer) handle(conn net.Conn) {
	defer conn.Close()

	encMu := &sync.Mutex{}
	enc := json.NewEncoder(conn)
	send := func(r state.Response) error {
		encMu.Lock()
		defer encMu.Unlock()
		return enc.Encode(r)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var subCh chan *state.Session
	defer func() {
		if subCh != nil {
			c.unsubscribe(subCh)
		}
	}()

	for scanner.Scan() {
		var req state.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = send(state.Response{Ok: false, Error: err.Error()})
			continue
		}
		switch req.Op {
		case state.OpGetState:
			_ = send(state.Response{Ok: true, Session: c.getState()})

		case state.OpSetAutoYes:
			if req.On != nil && c.onSetAutoYes != nil {
				c.onSetAutoYes(*req.On)
			}
			_ = send(state.Response{Ok: true, Session: c.getState()})

		case state.OpApproveNow:
			if c.onApproveNow != nil {
				c.onApproveNow()
			}
			_ = send(state.Response{Ok: true, Session: c.getState()})

		case state.OpSubscribe:
			if subCh == nil {
				subCh = make(chan *state.Session, 16)
				c.subscribe(subCh)
				_ = send(state.Response{Ok: true, Event: "snapshot", Session: c.getState()})
				go func() {
					for s := range subCh {
						if err := send(state.Response{Ok: true, Event: "state_changed", Session: s}); err != nil {
							return
						}
					}
				}()
			}

		default:
			_ = send(state.Response{Ok: false, Error: "unknown op: " + string(req.Op)})
		}
	}
}

func (c *controlServer) subscribe(ch chan *state.Session) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	c.subs = append(c.subs, ch)
}

func (c *controlServer) unsubscribe(ch chan *state.Session) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for i, s := range c.subs {
		if s == ch {
			c.subs = append(c.subs[:i], c.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Broadcast sends a state snapshot to all current subscribers. Non-blocking:
// if a subscriber's buffer is full we drop the update for that subscriber.
func (c *controlServer) Broadcast(s *state.Session) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- s:
		default:
		}
	}
}

func (c *controlServer) Close() {
	c.listener.Close()
	os.Remove(c.socketPath)
}
