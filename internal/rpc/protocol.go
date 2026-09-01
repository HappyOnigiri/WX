package rpc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	ProtocolVersion = 1
	maxFrame        = 8 << 20
)

type Request struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	Method         string          `json:"method"`
	Deadline       string          `json:"deadline,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Params         json.RawMessage `json:"params,omitempty"`
}
type Response struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type (
	RPCError struct{ Code, Message string }
	Handler  interface {
		Handle(context.Context, string, json.RawMessage) (any, error)
	}
)

type Client struct {
	Socket  string
	Timeout time.Duration
}

func (c Client) Call(ctx context.Context, method string, params, result any) error {
	return c.CallWithKey(ctx, method, "", params, result)
}

func (c Client) CallWithKey(ctx context.Context, method, idempotencyKey string, params, result any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connect to wx daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	deadline, hasDeadline := ctx.Deadline()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := Request{Version: ProtocolVersion, ID: id, Method: method, IdempotencyKey: idempotencyKey, Params: payload}
	if hasDeadline {
		req.Deadline = deadline.UTC().Format(time.RFC3339Nano)
	}
	if err := writeFrame(conn, req); err != nil {
		return err
	}
	var resp Response
	if err := readFrame(bufio.NewReader(conn), &resp); err != nil {
		return err
	}
	if resp.Version != ProtocolVersion {
		return errors.New("wx daemon protocol version mismatch")
	}
	if resp.ID != id {
		return errors.New("wx daemon response ID mismatch")
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if result != nil {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

type Server struct {
	Socket   string
	Handler  Handler
	listener net.Listener
	idemMu   sync.Mutex
	idem     map[string]*idempotentEntry
}

type idempotentEntry struct {
	method string
	params string
	done   chan struct{}
	result json.RawMessage
	err    *RPCError
}

func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(s.Socket), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(s.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket path %s", s.Socket)
		}
		probe, dialErr := net.DialTimeout("unix", s.Socket, 100*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return fmt.Errorf("RPC server is already listening on %s", s.Socket)
		}
		if err := os.Remove(s.Socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	s.listener = ln
	if err := os.Chmod(s.Socket, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				return err
			}
			return nil
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Server) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) serveConn(parent context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req Request
	if err := readFrame(bufio.NewReader(conn), &req); err != nil {
		return
	}
	resp := Response{Version: ProtocolVersion, ID: req.ID}
	if req.Version != ProtocolVersion {
		resp.Error = &RPCError{Code: "PROTOCOL_VERSION", Message: "unsupported protocol version"}
		_ = writeFrame(conn, resp)
		return
	}
	ctx := parent
	if req.Deadline != "" {
		if d, err := time.Parse(time.RFC3339Nano, req.Deadline); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(parent, d)
			defer cancel()
		}
	}
	if req.IdempotencyKey != "" {
		entry, owner := s.idempotencyEntry(req)
		if entry == nil {
			resp.Error = &RPCError{Code: "IDEMPOTENCY_KEY_REUSE", Message: "idempotency key was reused with a different method or payload"}
		} else if !owner {
			select {
			case <-entry.done:
				resp.Result, resp.Error = entry.result, entry.err
			case <-ctx.Done():
				resp.Error = &RPCError{Code: "DEADLINE", Message: ctx.Err().Error()}
			}
		} else {
			resp.Result, resp.Error = s.invoke(ctx, req)
			s.idemMu.Lock()
			entry.result, entry.err = resp.Result, resp.Error
			close(entry.done)
			s.idemMu.Unlock()
		}
	} else {
		resp.Result, resp.Error = s.invoke(ctx, req)
	}
	_ = writeFrame(conn, resp)
}

func (s *Server) invoke(ctx context.Context, req Request) (json.RawMessage, *RPCError) {
	result, err := s.Handler.Handle(ctx, req.Method, req.Params)
	if err != nil {
		return nil, &RPCError{Code: "REQUEST_FAILED", Message: err.Error()}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, &RPCError{Code: "ENCODE_FAILED", Message: err.Error()}
	}
	return encoded, nil
}

func (s *Server) idempotencyEntry(req Request) (*idempotentEntry, bool) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	if s.idem == nil {
		s.idem = map[string]*idempotentEntry{}
	}
	if existing, ok := s.idem[req.IdempotencyKey]; ok {
		if existing.method != req.Method || existing.params != string(req.Params) {
			return nil, false
		}
		return existing, false
	}
	entry := &idempotentEntry{method: req.Method, params: string(req.Params), done: make(chan struct{})}
	s.idem[req.IdempotencyKey] = entry
	return entry, true
}

func writeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxFrame {
		return errors.New("RPC frame too large")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func readFrame(r io.Reader, v any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxFrame {
		return errors.New("invalid RPC frame length")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
