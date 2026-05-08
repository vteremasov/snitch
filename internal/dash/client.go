package dash

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"time"

	"snitch/internal/state"
)

// Client wraps a unix-socket connection to a single `snitch run`. On Dial we
// open the connection and immediately subscribe, then a single read loop
// drains every server message into Updates. SetAutoYes / ApproveNow are
// fire-and-forget on the write side — the wrapper's response will surface
// through the same Updates channel as a state_changed event.
type Client struct {
	pid  int
	path string

	writeMu sync.Mutex
	conn    net.Conn
	enc     *json.Encoder

	updates chan *state.Session
}

func Dial(socketPath string, pid int) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 1*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{
		pid:     pid,
		path:    socketPath,
		conn:    conn,
		enc:     json.NewEncoder(conn),
		updates: make(chan *state.Session, 16),
	}
	if err := c.enc.Encode(state.Request{Op: state.OpSubscribe}); err != nil {
		conn.Close()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) PID() int                       { return c.pid }
func (c *Client) Updates() <-chan *state.Session { return c.updates }

func (c *Client) Close() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *Client) SetAutoYes(on bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return net.ErrClosed
	}
	return c.enc.Encode(state.Request{Op: state.OpSetAutoYes, On: &on})
}

func (c *Client) ApproveNow() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return net.ErrClosed
	}
	return c.enc.Encode(state.Request{Op: state.OpApproveNow})
}

func (c *Client) readLoop() {
	defer close(c.updates)
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var resp state.Response
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}
		if resp.Session == nil {
			continue
		}
		select {
		case c.updates <- resp.Session:
		default:
			// Backlog is bounded; the dash will see the next push.
		}
	}
}
