package executor

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Mirrors the native handler's dependency: an early retryable rejection must
// reach the conductor without permanently signalling this downstream session
// as disconnected. It does not claim to reproduce the native client's UI loop.
func TestCodexWebsocketsExecutor_PreOutputQuotaPreservesDownstream(t *testing.T) {
	frames := map[string]string{
		"error":  `{ "type":"error", "status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":600}}`,
		"failed": `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","status_code":429,"message":"The usage limit has been reached","resets_in_seconds":600}}}`,
	}
	for name, frame := range frames {
		for _, scenario := range []struct {
			name      string
			buffering bool
			preamble  []string
		}{
			{name: "immediate-unbuffered"},
			{name: "immediate-buffered", buffering: true},
			{name: "handshake-buffered", buffering: true, preamble: []string{codexCreatedEvent, codexInProgressEvent}},
		} {
			t.Run(fmt.Sprintf("%s/%s", name, scenario.name), func(t *testing.T) {
				frames := append(append([]string{}, scenario.preamble...), frame)
				server := codexWebsocketServerHoldingConnection(t, frames...)
				defer server.Close()
				executor := NewCodexWebsocketsExecutor(codexBufferingConfig(scenario.buffering))
				executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				const session = "synthetic-quota-recovery"
				disconnect := executor.UpstreamDisconnectChan(session)
				if disconnect == nil {
					t.Fatal("missing disconnect channel")
				}
				req, opts := codexWebsocketRequest()
				opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}
				ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
				result, streamErr := executor.ExecuteStream(ctx, codexTestAuth(server.URL), req, opts)
				if result != nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
				}
				if streamErr == nil || statusCodeFromTestError(t, streamErr) != http.StatusTooManyRequests {
					t.Fatalf("expected synthetic 429, got %v", streamErr)
				}
				select {
				case <-disconnect:
					t.Fatal("early quota rejection permanently signalled downstream disconnect before any payload")
				default:
				}
			})
		}
	}
}

func TestCodexWebsocketsExecutor_PostOutputQuotaStillNotifiesDisconnect(t *testing.T) {
	server := codexWebsocketServerHoldingConnection(t, codexCreatedEvent, codexOutputDeltaEvent,
		`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":600}}`)
	defer server.Close()
	executor := NewCodexWebsocketsExecutor(codexBufferingConfig(false))
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const session = "synthetic-post-output-quota"
	disconnect := executor.UpstreamDisconnectChan(session)
	req, opts := codexWebsocketRequest()
	opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}
	result, err := executor.ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	sawPayload, sawError := false, false
	for chunk := range result.Chunks {
		sawPayload = sawPayload || len(chunk.Payload) > 0
		sawError = sawError || chunk.Err != nil
	}
	if !sawPayload || !sawError {
		t.Fatalf("expected partial payload then error; payload=%t error=%t", sawPayload, sawError)
	}
	select {
	case <-disconnect:
	default:
		t.Fatal("post-output quota error lost disconnect notification")
	}
}

func TestCodexWebsocketsExecutor_TranslatedZeroOutputQuotaPreservesDownstream(t *testing.T) {
	server := codexWebsocketServerHoldingConnection(t, codexCreatedEvent,
		`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":600}}`)
	defer server.Close()
	executor := NewCodexWebsocketsExecutor(codexBufferingConfig(false))
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const session = "synthetic-translated-zero-output"
	disconnect := executor.UpstreamDisconnectChan(session)
	req, opts := codexWebsocketRequest()
	opts.ResponseFormat = sdktranslator.FromString("openai")
	opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}
	result, streamErr := executor.ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if result != nil {
		for chunk := range result.Chunks {
			if len(chunk.Payload) > 0 {
				t.Fatal("fixture unexpectedly produced translated output")
			}
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
		}
	}
	if streamErr == nil || statusCodeFromTestError(t, streamErr) != http.StatusTooManyRequests {
		t.Fatalf("expected synthetic429, got %v", streamErr)
	}
	select {
	case <-disconnect:
		t.Fatal("translated zero-output preamble incorrectly counted as emitted payload")
	default:
	}
}
