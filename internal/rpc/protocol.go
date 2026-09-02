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
	ProtocolVersion             = 1
	maxFrame                    = 8 << 20
	maxIdempotency              = 2048
	idempotencyTTL              = 24 * time.Hour
	defaultClientTimeout        = 10 * time.Second
	defaultServerFrameTimeout   = 10 * time.Second
	defaultServerHandlerTimeout = 10 * time.Second
	serverResponseGrace         = 100 * time.Millisecond
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
	DurableIdempotency interface {
		BeginRPCRequest(context.Context, string, string, string, time.Time) ([]byte, string, string, bool, error)
		CompleteRPCRequest(context.Context, string, string, string, []byte, string, string, time.Time) error
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
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.callOnce(ctx, method, idempotencyKey, params, result)
		if err == nil || idempotencyKey == "" || !transientTransportError(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(25*(1<<attempt)) * time.Millisecond):
		}
	}
	return err
}

func (c Client) callOnce(ctx context.Context, method, idempotencyKey string, params, result any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultClientTimeout
	}
	ioCtx := ctx
	stopTimeout := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ioCtx, cancel = context.WithTimeout(ctx, timeout)
		stopTimeout = cancel
	}
	defer stopTimeout()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ioCtx, "unix", c.Socket)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("connect to wx daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stopConnection := watchContext(ioCtx, conn)
	defer stopConnection()
	if deadline, ok := ioCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	deadline, hasDeadline := ioCtx.Deadline()
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := Request{Version: ProtocolVersion, ID: id, Method: method, IdempotencyKey: idempotencyKey, Params: payload}
	if hasDeadline {
		req.Deadline = deadline.UTC().Format(time.RFC3339Nano)
	}
	if err := writeFrame(conn, req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	var resp Response
	if err := readFrame(bufio.NewReader(conn), &resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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

func watchContext(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	if ctx.Done() == nil {
		return func() {}
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func transientTransportError(err error) bool {
	var networkError *net.OpError
	return errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

type Server struct {
	Socket         string
	Handler        Handler
	Durable        DurableIdempotency
	FrameTimeout   time.Duration
	HandlerTimeout time.Duration
	listener       net.Listener
	idemMu         sync.Mutex
	idem           map[string]*idempotentEntry
}

type idempotentEntry struct {
	method string
	params string
	done   chan struct{}
	result json.RawMessage
	err    *RPCError
	ended  time.Time
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
		probe, dialErr := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "unix", s.Socket)
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
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", s.Socket)
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
	stopContext := watchContext(parent, conn)
	defer stopContext()
	frameDeadline := time.Now().Add(s.frameTimeout())
	if err := conn.SetReadDeadline(frameDeadline); err != nil {
		return
	}
	var req Request
	if err := readFrame(bufio.NewReader(conn), &req); err != nil {
		return
	}
	resp := Response{Version: ProtocolVersion, ID: req.ID}
	if req.Version != ProtocolVersion {
		resp.Error = &RPCError{Code: "PROTOCOL_VERSION", Message: "unsupported protocol version"}
		_ = conn.SetWriteDeadline(time.Now().Add(s.frameTimeout()))
		_ = writeFrame(conn, resp)
		return
	}
	handlerDeadline, err := s.requestDeadline(parent, req.Deadline)
	if err != nil {
		resp.Error = &RPCError{Code: "INVALID_DEADLINE", Message: "invalid request deadline"}
		_ = conn.SetWriteDeadline(time.Now().Add(s.frameTimeout()))
		_ = writeFrame(conn, resp)
		return
	}
	if err := conn.SetDeadline(handlerDeadline.Add(serverResponseGrace)); err != nil {
		return
	}
	ctx, cancel := context.WithDeadline(parent, handlerDeadline)
	defer cancel()
	if req.IdempotencyKey != "" {
		entry, owner := s.idempotencyEntry(req)
		switch {
		case entry == nil:
			resp.Error = &RPCError{Code: "IDEMPOTENCY_KEY_REUSE", Message: "idempotency key was reused with a different method or payload"}
		case !owner:
			select {
			case <-entry.done:
				resp.Result, resp.Error = entry.result, entry.err
			case <-ctx.Done():
				resp.Error = &RPCError{Code: "DEADLINE", Message: ctx.Err().Error()}
			}
		default:
			if s.Durable != nil {
				result, errorCode, errorMessage, execute, err := s.Durable.BeginRPCRequest(ctx, req.IdempotencyKey, req.Method, string(req.Params), time.Now().Add(idempotencyTTL))
				if err != nil {
					resp.Error = &RPCError{Code: "IDEMPOTENCY_STORE", Message: "durable idempotency reservation failed"}
					s.finishIdempotencyEntry(req.IdempotencyKey, entry, resp, false)
					_ = writeFrame(conn, resp)
					return
				}
				if !execute {
					resp.Result = result
					if errorCode != "" {
						resp.Error = &RPCError{Code: errorCode, Message: errorMessage}
					}
					s.finishIdempotencyEntry(req.IdempotencyKey, entry, resp, true)
					_ = writeFrame(conn, resp)
					return
				}
			}
			resp.Result, resp.Error = s.invoke(ctx, req)
			if s.Durable != nil {
				errorCode, errorMessage := "", ""
				if resp.Error != nil {
					errorCode, errorMessage = resp.Error.Code, resp.Error.Message
				}
				persistCtx, cancel := context.WithTimeout(parent, 2*time.Second)
				err := s.Durable.CompleteRPCRequest(persistCtx, req.IdempotencyKey, req.Method, string(req.Params), resp.Result, errorCode, errorMessage, time.Now().Add(idempotencyTTL))
				cancel()
				if err != nil {
					resp.Result = nil
					resp.Error = &RPCError{Code: "IDEMPOTENCY_STORE", Message: "durable idempotency result could not be committed"}
				}
			}
			s.finishIdempotencyEntry(req.IdempotencyKey, entry, resp, true)
		}
	} else {
		resp.Result, resp.Error = s.invoke(ctx, req)
	}
	_ = writeFrame(conn, resp)
}

func (s *Server) frameTimeout() time.Duration {
	if s.FrameTimeout > 0 {
		return s.FrameTimeout
	}
	return defaultServerFrameTimeout
}

func (s *Server) handlerTimeout() time.Duration {
	if s.HandlerTimeout > 0 {
		return s.HandlerTimeout
	}
	return defaultServerHandlerTimeout
}

func (s *Server) requestDeadline(parent context.Context, requested string) (time.Time, error) {
	now := time.Now()
	deadline := now.Add(s.handlerTimeout())
	if requested != "" {
		requestedDeadline, err := time.Parse(time.RFC3339Nano, requested)
		if err != nil {
			return time.Time{}, err
		}
		deadline = requestedDeadline
	}
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return deadline, nil
}

func (s *Server) finishIdempotencyEntry(key string, entry *idempotentEntry, response Response, retain bool) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	entry.result, entry.err = response.Result, response.Error
	entry.ended = time.Now()
	close(entry.done)
	if !retain && s.idem[key] == entry {
		delete(s.idem, key)
	}
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
	s.pruneIdempotencyLocked(time.Now())
	if len(s.idem) >= maxIdempotency {
		return nil, false
	}
	entry := &idempotentEntry{method: req.Method, params: string(req.Params), done: make(chan struct{})}
	s.idem[req.IdempotencyKey] = entry
	return entry, true
}

func (s *Server) pruneIdempotencyLocked(now time.Time) {
	for key, entry := range s.idem {
		if !entry.ended.IsZero() && now.Sub(entry.ended) >= idempotencyTTL {
			delete(s.idem, key)
		}
	}
	for len(s.idem) >= maxIdempotency {
		var oldestKey string
		var oldest time.Time
		for key, entry := range s.idem {
			if entry.ended.IsZero() || !oldest.IsZero() && !entry.ended.Before(oldest) {
				continue
			}
			oldestKey, oldest = key, entry.ended
		}
		if oldestKey == "" {
			return
		}
		delete(s.idem, oldestKey)
	}
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
