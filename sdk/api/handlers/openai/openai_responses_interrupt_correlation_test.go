package openai

import (
	"bytes"
	"testing"
)

func TestResponsesLocalInterruptRejectsUnidentifiedResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	local.begin(false)
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"unknown"}`)) {
		t.Fatal("unidentified response accepted an arbitrary interrupt")
	}
}

func TestResponsesLocalInterruptCorrelatesActiveResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	valid := []byte(`{"type":"response.interrupt","response_id":"r1","extension":"preserve"}`)
	unknown := []byte(`{"type":"response.interrupt","response_id":"unknown"}`)
	local.begin(false)
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
	local.begin(false)
	local.observe([]byte(`{"type":"response.created","response":{"id":"r2"}}`))
	if local.deliver(valid) {
		t.Fatal("prior response canceled its successor")
	}
	if !local.deliver([]byte(`{"type":"response.interrupt","response_id":"r2"}`)) {
		t.Fatal("successor active interrupt rejected")
	}
	// Upstream keeps a claimed frame queued at turn end so the forward loop
	// answers it; once taken, the next turn starts without a queued control.
	local.end()
	if got := local.takeClaimed(); got == nil {
		t.Fatal("claimed interrupt was dropped at turn end")
	}
	local.begin(false)
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
			local.begin(false)
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
