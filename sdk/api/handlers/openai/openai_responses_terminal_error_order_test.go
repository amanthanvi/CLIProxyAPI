package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesWebsocketDrainsTerminalErrorBeforeEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	results := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			results <- err
			return
		}
		defer func() { _ = conn.Close() }()
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		data := make(chan []byte)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: websocketPinnedFailoverStatusError{
			status: http.StatusBadRequest,
			msg:    `{"error":{"type":"invalid_request_error","message":"Correct the request"}}`,
		}}
		// Match the broker: publish the terminal error, then close errors and data.
		close(errs)
		close(data)
		h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
		_, _, _, errMsg, errForward := h.forwardResponsesWebsocket(ctx, newResponsesWebsocketWriter(conn), func(...interface{}) {}, data, errs, nil, "terminal-error-order")
		if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest || !errors.Is(errForward, websocket.ErrCloseSent) {
			results <- fmt.Errorf("queued terminal request error lost: error=%v forward=%v", errMsg, errForward)
			return
		}
		results <- nil
		// A successful result must close the socket itself, not via fixture cleanup.
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	// Both channels are ready on every attempt; every scheduler choice must work.
	for attempt := 0; attempt < 64; attempt++ {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, payload, readErr := conn.ReadMessage()
		_, _, closeErr := conn.ReadMessage()
		_ = conn.Close()
		var serverErr error
		select {
		case serverErr = <-results:
		case <-time.After(3 * time.Second):
			t.Fatal("terminal error forwarder did not finish")
		}
		if readErr != nil || gjson.GetBytes(payload, "type").String() != "error" || gjson.GetBytes(payload, "status").Int() != http.StatusBadRequest || gjson.GetBytes(payload, "error.message").String() != "Correct the request" {
			t.Fatalf("attempt %d lost terminal error before close: read=%v server=%v", attempt, readErr, serverErr)
		}
		if closeErr == nil || serverErr != nil {
			t.Fatalf("attempt %d did not emit exactly one terminal error and close: close=%v server=%v", attempt, closeErr, serverErr)
		}
		if netErr, ok := closeErr.(interface{ Timeout() bool }); ok && netErr.Timeout() {
			t.Fatalf("attempt %d left the socket open after the terminal error", attempt)
		}
	}
}
