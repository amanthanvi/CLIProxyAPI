package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestInterruptExecutionSessionCorrelatesResponseID(t *testing.T) {
	valid1 := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","extension":"preserve"}`)
	valid2 := []byte(`{"type":"response.interrupt","response_id":"r2","mode":"discard_partial_items","extension":"preserve"}`)
	barrier := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		write := func(payload string) bool {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Errorf("upstream write: %v", err)
				return false
			}
			return true
		}
		read := func(want []byte) bool {
			_, got, err := conn.ReadMessage()
			if err != nil {
				return false
			}
			if !bytes.Equal(got, want) {
				t.Errorf("unexpected control frame: %s", got)
				return false
			}
			return true
		}
		if !write(`{"type":"response.created","response":{"id":"r1"}}`) || !read(valid1) || !write(`{"type":"response.completed","response":{"id":"r1","output":[]}}`) || !read([]byte("next")) || !write(`{"type":"response.created","response":{"id":"r2"}}`) || !read(valid2) || !write(`{"type":"response.completed","response":{"id":"r2","output":[]}}`) || !read([]byte("barrier")) {
			return
		}
		close(barrier)
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := NewCodexWebsocketsExecutor(nil)
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const sessionID = "interrupt-id-correlation"
	defer exec.CloseExecutionSession(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.conn = conn
	sess.connCloser = newWebsocketConnectionCloser(conn)
	sess.configureConn(conn)
	readCh := sess.activate(conn)
	go exec.readUpstreamLoop(sess, conn)
	read := func() {
		t.Helper()
		select {
		case event := <-readCh:
			if event.err != nil {
				t.Fatal(event.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("response event timed out")
		}
	}
	rejectUnknown := func() {
		t.Helper()
		err := exec.InterruptExecutionSession(context.Background(), sessionID, []byte(`{"type":"response.interrupt","response_id":"unknown"}`))
		if err == nil || errors.Is(err, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
			t.Fatalf("unknown response must be rejected without HTTP fallback, got %v", err)
		}
	}
	interrupt := func(payload []byte) {
		t.Helper()
		if err := exec.InterruptExecutionSession(context.Background(), sessionID, payload); err != nil {
			t.Fatal(err)
		}
	}
	read()
	rejectUnknown()
	interrupt(valid1)
	read()
	// Keep the terminal turn's read channel live to pin its cleanup race.
	rejectUnknown()
	interrupt(valid1)
	sess.clearActive(conn, readCh)
	readCh = sess.activate(conn)
	rejectUnknown()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("next")); err != nil {
		t.Fatal(err)
	}
	read()
	rejectUnknown()
	interrupt(valid1)
	interrupt(valid2)
	read()
	interrupt(valid2)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("barrier")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrier:
	case <-time.After(3 * time.Second):
		t.Fatal("control ordering barrier timed out")
	}
}

func TestInterruptExecutionSessionCompletesBeforeControlWrite(t *testing.T) {
	complete := make(chan struct{})
	var completeOnce sync.Once
	finish := func() { completeOnce.Do(func() { close(complete) }) }
	observed := make(chan int, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1"}}`)); err != nil {
			t.Error(err)
			return
		}
		<-complete
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`)); err != nil {
			t.Error(err)
			return
		}
		count := 0
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if bytes.Equal(payload, []byte("barrier")) {
				observed <- count
				return
			}
			count++
		}
	}))
	defer upstream.Close()
	defer finish()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := NewCodexWebsocketsExecutor(nil)
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const sessionID = "terminal-before-control-write"
	defer exec.CloseExecutionSession(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.conn = conn
	sess.connCloser = newWebsocketConnectionCloser(conn)
	sess.configureConn(conn)
	readCh := sess.activate(conn)
	go exec.readUpstreamLoop(sess, conn)
	read := func() {
		t.Helper()
		select {
		case event := <-readCh:
			if event.err != nil {
				t.Fatal(event.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("response event timed out")
		}
	}
	read()
	// Pin completion after the interrupt reaches the writer, before any bytes.
	testWebsocketWritePayloadHook = func(*websocket.Conn) {
		finish()
		read()
	}
	defer func() { testWebsocketWritePayloadHook = nil }()
	if err := exec.InterruptExecutionSession(context.Background(), sessionID, []byte(`{"type":"response.interrupt","response_id":"r1"}`)); err != nil {
		t.Fatal(err)
	}
	testWebsocketWritePayloadHook = nil
	if err := conn.WriteMessage(websocket.TextMessage, []byte("barrier")); err != nil {
		t.Fatal(err)
	}
	select {
	case count := <-observed:
		if count != 0 {
			t.Fatalf("forwarded %d interrupt(s) after terminal state reached the writer", count)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control write barrier timed out")
	}
}
