package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var errConnClosed = errors.New("mpv ipc closed")

type ipcReply struct {
	RequestID uint64          `json:"request_id"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

type ipcEvent struct {
	Event string          `json:"event"`
	Name  string          `json:"name"`
	Data  json.RawMessage `json:"data"`
	ID    uint64          `json:"id"`
}

// ipcClient is a single mpv JSON-IPC connection. One reader goroutine routes
// replies to waiting calls and pushes async events to Events (lossy on full).
// All writes go through wmu, so frames never interleave.
type ipcClient struct {
	conn    net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan ipcReply
	Events  chan ipcEvent
	closed  chan struct{}
	once    sync.Once
}

func dialIPC(sock string) (*ipcClient, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c := &ipcClient{
		conn:    conn,
		pending: make(map[uint64]chan ipcReply),
		Events:  make(chan ipcEvent, 64),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *ipcClient) readLoop() {
	r := bufio.NewReaderSize(c.conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			c.shutdown()
			return
		}
	}
}

func (c *ipcClient) dispatch(line []byte) {
	var probe struct {
		Event     string  `json:"event"`
		RequestID *uint64 `json:"request_id"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return
	}
	if probe.RequestID != nil {
		var rep ipcReply
		if json.Unmarshal(line, &rep) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[*probe.RequestID]
		delete(c.pending, *probe.RequestID)
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- rep:
			default:
			}
		}
		return
	}
	if probe.Event != "" {
		var ev ipcEvent
		if json.Unmarshal(line, &ev) == nil {
			select {
			case c.Events <- ev:
			default: // property events are lossy-tolerable
			}
		}
	}
}

func (c *ipcClient) shutdown() {
	c.once.Do(func() { close(c.closed) })
	c.conn.Close()
}

func (c *ipcClient) Close() { c.shutdown() }

// Call sends one command and waits for its correlated reply.
func (c *ipcClient) Call(ctx context.Context, argv ...any) (json.RawMessage, string, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan ipcReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	frame, err := json.Marshal(map[string]any{"command": argv, "request_id": id})
	if err != nil {
		return nil, "", err
	}
	c.wmu.Lock()
	_, werr := c.conn.Write(append(frame, '\n'))
	c.wmu.Unlock()
	if werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, "", werr
	}

	select {
	case rep := <-ch:
		return rep.Data, rep.Error, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, "", ctx.Err()
	case <-c.closed:
		return nil, "", errConnClosed
	}
}

// Prop returns a property's raw value; mpv answers error:"success" on success.
func (c *ipcClient) Prop(ctx context.Context, name string) (json.RawMessage, error) {
	data, errstr, err := c.Call(ctx, "get_property", name)
	if err != nil {
		return nil, err
	}
	if errstr != "" && errstr != "success" {
		return nil, errors.New(errstr)
	}
	return data, nil
}

func (c *ipcClient) PropString(ctx context.Context, name string) (string, error) {
	raw, err := c.Prop(ctx, name)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("%s: not a string", name)
	}
	return s, nil
}

func (c *ipcClient) PropFloat(ctx context.Context, name string) (float64, error) {
	raw, err := c.Prop(ctx, name)
	if err != nil {
		return 0, err
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return 0, fmt.Errorf("%s: not a number", name)
		}
		f, _ = n.Float64()
	}
	return f, nil
}

func (c *ipcClient) PropInt(ctx context.Context, name string) (int, error) {
	f, err := c.PropFloat(ctx, name)
	return int(f), err
}

func (c *ipcClient) PropBool(ctx context.Context, name string) (bool, error) {
	raw, err := c.Prop(ctx, name)
	if err != nil {
		return false, err
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, fmt.Errorf("%s: not a bool", name)
	}
	return b, nil
}

// Observe registers a property observer. It takes a ctx because a wedged mpv
// must not be able to hang the caller for an unbounded time.
func (c *ipcClient) Observe(ctx context.Context, id int, name string) error {
	_, errstr, err := c.Call(ctx, "observe_property", id, name)
	if err != nil {
		return err
	}
	if errstr != "" && errstr != "success" {
		return errors.New(errstr)
	}
	return nil
}
