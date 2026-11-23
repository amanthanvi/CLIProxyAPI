package api

import (
	"context"
	"net"
	"net/http"
	"sync"
)

type requestAdmission struct {
	mu              sync.Mutex
	stopping        bool
	active          int
	idle            chan struct{}
	hijacked        map[net.Conn]struct{}
	closingHijacked bool
}

type drainConnectionKey struct{}

func (s *Server) trackConnectionState(conn net.Conn, state http.ConnState) {
	if state != http.StateHijacked && state != http.StateClosed {
		return
	}
	s.admission.mu.Lock()
	if state == http.StateHijacked {
		if s.admission.hijacked == nil {
			s.admission.hijacked = make(map[net.Conn]struct{})
		}
		s.admission.hijacked[conn] = struct{}{}
	} else {
		delete(s.admission.hijacked, conn)
	}
	closeConn := state == http.StateHijacked && s.admission.closingHijacked
	s.admission.mu.Unlock()
	if closeConn {
		_ = conn.Close()
	}
}

func (s *Server) trackAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.admission.mu.Lock()
		if s.admission.stopping {
			s.admission.mu.Unlock()
			w.Header().Set("Connection", "close")
			http.Error(w, "service shutting down", http.StatusServiceUnavailable)
			return
		}
		if s.admission.active == 0 {
			s.admission.idle = make(chan struct{})
		}
		s.admission.active++
		s.admission.mu.Unlock()
		defer func() {
			s.admission.mu.Lock()
			if conn, ok := r.Context().Value(drainConnectionKey{}).(net.Conn); ok {
				delete(s.admission.hijacked, conn)
			}
			s.admission.active--
			if s.admission.active == 0 {
				close(s.admission.idle)
			}
			s.admission.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// BeginDrain rejects new HTTP requests without cancelling established work.
func (s *Server) BeginDrain() {
	if s == nil {
		return
	}
	s.admission.mu.Lock()
	s.admission.stopping = true
	s.admission.mu.Unlock()
}

// Drain stops admission, closes hijacked sockets, and waits for all handlers.
// Callers must first quiesce executions on hijacked sockets to avoid cancelling active work.
// A failed drain is not a successful final-checkpoint boundary. Stop remains immediate.
func (s *Server) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.BeginDrain()
	s.admission.mu.Lock()
	s.admission.closingHijacked = true
	connections := make([]net.Conn, 0, len(s.admission.hijacked))
	for conn := range s.admission.hijacked {
		connections = append(connections, conn)
	}
	idle := s.admission.idle
	s.admission.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	if s.server != nil {
		if err := s.server.Shutdown(ctx); err != nil {
			return err
		}
	}
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
