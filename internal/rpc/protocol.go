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
	"time"
)

const ProtocolVersion = 1
const maxFrame = 8 << 20

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
type RPCError struct{ Code, Message string }
type Handler interface {
	Handle(context.Context, string, json.RawMessage) (any, error)
}

type Client struct {
	Socket  string
	Timeout time.Duration
}

func (c Client) Call(ctx context.Context, method string, params, result any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connect to wx daemon: %w", err)
	}
	defer conn.Close()
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	deadline, hasDeadline := ctx.Deadline()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := Request{Version: ProtocolVersion, ID: id, Method: method, Params: payload}
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
}

func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(s.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket path %s", s.Socket)
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
	if err := os.Chmod(s.Socket, 0600); err != nil {
		ln.Close()
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
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
	defer conn.Close()
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
	result, err := s.Handler.Handle(ctx, req.Method, req.Params)
	if err != nil {
		resp.Error = &RPCError{Code: "REQUEST_FAILED", Message: err.Error()}
	} else {
		resp.Result, _ = json.Marshal(result)
	}
	_ = writeFrame(conn, resp)
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
