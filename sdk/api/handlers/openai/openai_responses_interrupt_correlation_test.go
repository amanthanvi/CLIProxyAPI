package openai

import (
	"bytes"
	"testing"
)

func TestResponsesLocalInterruptRejectsUnidentifiedResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	local.begin()
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"unknown"}`)) {
		t.Fatal("unidentified response accepted an arbitrary interrupt")
	}
}

func TestResponsesLocalInterruptCorrelatesActiveResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	valid := []byte(`{"type":"response.interrupt","response_id":"r1","extension":"preserve"}`)
	unknown := []byte(`{"type":"response.interrupt","response_id":"unknown"}`)
	local.begin()
	local.observe([]byte(`{"type":"response.created","response":{"id":"r1"}}`))
	if local.deliver(unknown) {
		t.Fatal("unknown response canceled the active response")
	}
	select {
	case <-local.framesChan():
		t.Fatal("rejected interrupt queued a frame")
	default:
	}
	if !local.deliver(valid) {
		t.Fatal("valid active interrupt rejected")
	}
	if local.deliver(valid) {
		t.Fatal("duplicate interrupt queued a second cancellation")
	}
	if got := <-local.framesChan(); !bytes.Equal(got, valid) {
		t.Fatal("valid interrupt frame was modified")
	}
	local.end()
	if local.deliver(valid) {
		t.Fatal("idle state accepted an interrupt")
	}
	local.begin()
	local.observe([]byte(`{"type":"response.created","response":{"id":"r2"}}`))
	if local.deliver(valid) {
		t.Fatal("prior response canceled its successor")
	}
	if !local.deliver([]byte(`{"type":"response.interrupt","response_id":"r2"}`)) {
		t.Fatal("successor active interrupt rejected")
	}
	// Ending a turn clears queued controls before the next response starts.
	local.end()
	local.begin()
	select {
	case <-local.framesChan():
		t.Fatal("successor retained a prior queued interrupt")
	default:
	}
}

func TestResponsesLocalInterruptTerminalBeforeDelivery(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete", "response.failed"} {
		t.Run(terminal, func(t *testing.T) {
			local := newResponsesLocalInterrupt()
			local.begin()
			local.observe([]byte(`{"type":"response.created","response":{"id":"r1"}}`))
			local.observe([]byte(`{"type":"` + terminal + `","response":{"id":"r1"}}`))
			for _, id := range []string{"r1", "unknown"} {
				if local.deliver([]byte(`{"type":"response.interrupt","response_id":"` + id + `"}`)) {
					t.Fatalf("terminal cleanup accepted an interrupt for %s", id)
				}
			}
		})
	}
}
