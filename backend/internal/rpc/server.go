package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const MaxFrame = 256 * 1024

type request struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}
type Server struct {
	listener *net.UnixListener
	app      *app.App
	mu       sync.Mutex
	clients  map[*net.UnixConn]bool
	wg       sync.WaitGroup
}

func Listen(path string, a *app.App) (*Server, error) {
	if !filepath.IsAbs(path) || len(path) > 103 {
		return nil, fmt.Errorf("socket path must be absolute and at most 103 bytes")
	}
	parent := filepath.Dir(path)
	if e := os.MkdirAll(parent, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(parent)
	if e != nil {
		return nil, e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("socket directory must be private (0700)")
	}
	l, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if e != nil {
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		l.Close()
		return nil, e
	}
	return &Server{listener: l, app: a, clients: map[*net.UnixConn]bool{}}, nil
}
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.listener.Close()
		s.mu.Lock()
		for c := range s.clients {
			c.Close()
		}
		s.mu.Unlock()
	}()
	defer s.wg.Wait()
	for {
		c, e := s.listener.AcceptUnix()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		if !sameUID(c) {
			c.Close()
			continue
		}
		s.mu.Lock()
		if len(s.clients) >= 16 {
			s.mu.Unlock()
			c.Close()
			continue
		}
		s.clients[c] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(ctx, c)
	}
}
func sameUID(c *net.UnixConn) bool {
	r, e := c.SyscallConn()
	if e != nil {
		return false
	}
	ok := false
	e = r.Control(func(fd uintptr) {
		u, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		ok = e == nil && int(u.Uid) == os.Getuid()
	})
	return e == nil && ok
}
func (s *Server) serve(ctx context.Context, c *net.UnixConn) {
	defer s.wg.Done()
	defer func() { c.Close(); s.mu.Lock(); delete(s.clients, c); s.mu.Unlock() }()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), MaxFrame)
	for {
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		if !scan.Scan() {
			return
		}
		var q request
		var result any
		var err error
		if e := json.Unmarshal(scan.Bytes(), &q); e != nil || q.V != 1 || q.ID == "" || len(q.ID) > 128 {
			err = &app.Error{Code: "invalid_request", Message: "Expected v1 request with string id"}
		} else {
			call, cancel := context.WithTimeout(ctx, 25*time.Second)
			result, err = s.app.Handle(call, q.Method, q.Params)
			cancel()
		}
		response := map[string]any{"v": 1, "id": q.ID}
		if err != nil {
			var ae *app.Error
			if !errors.As(err, &ae) {
				ae = &app.Error{Code: "internal", Message: "Local operation failed"}
			}
			response["error"] = ae
		} else {
			response["result"] = result
		}
		data, e := json.Marshal(response)
		if e != nil {
			return
		}
		if len(data)+1 > MaxFrame {
			data, _ = json.Marshal(map[string]any{"v": 1, "id": q.ID, "error": &app.Error{Code: "response_too_large", Message: "Request a smaller page"}})
		}
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, e = c.Write(append(data, '\n')); e != nil {
			return
		}
	}
}
