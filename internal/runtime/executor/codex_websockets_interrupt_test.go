package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestInterruptExecutionSessionRequiresActiveRead(t *testing.T) {
	received := make(chan []byte, 1)
	startResponse := make(chan struct{})
	var startOnce sync.Once
	start := func() { startOnce.Do(func() { close(startResponse) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		<-startResponse
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1"}}`)); errWrite != nil {
			return
		}
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			return
		}
		received <- payload
	}))
	defer upstream.Close()
	defer start()

	client, _, errDial := websocket.DefaultDialer.Dial("ws"+upstream.URL[len("http"):], nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()

	const sessionID = "interrupt-requires-active-read"
	executor := NewCodexWebsocketsExecutor(nil)
	defer executor.CloseExecutionSession(sessionID)
	sess := executor.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = client
	sess.authID = "auth"
	sess.wsURL = upstream.URL
	sess.connMu.Unlock()
	sess.configureConn(client)

	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	errIdle := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt)
	if !errors.Is(errIdle, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
		t.Fatalf("idle socket error = %v, want ErrNoActiveUpstreamWebsocket", errIdle)
	}
	select {
	case payload := <-received:
		t.Fatalf("idle socket was written: %s", payload)
	case <-time.After(200 * time.Millisecond):
	}

	readCh := sess.activate(client)
	defer sess.clearActive(client, readCh)
	go executor.readUpstreamLoop(sess, client)
	start()
	select {
	case event := <-readCh:
		if event.err != nil || !bytes.Equal(event.payload, []byte(`{"type":"response.created","response":{"id":"r1"}}`)) {
			t.Fatalf("active response was not established: %v", event.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active response.created timed out")
	}
	if errActive := executor.InterruptExecutionSession(context.Background(), sessionID, interrupt); errActive != nil {
		t.Fatal(errActive)
	}
	select {
	case payload := <-received:
		if !bytes.Equal(payload, interrupt) {
			t.Fatalf("active interrupt = %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active socket did not receive the interrupt")
	}
}
