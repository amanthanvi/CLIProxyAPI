# Native Responses Interrupt Backport

This managed change retains the v7.3.16-slotclear.4 baseline, including consumed
usage-slot clearing, deferred WebSocket body suppression, Fast/effective-tier
translation, Claude metadata synchronization, and scoped native Claude routing.
It does not migrate gateway configuration or management clients to v8.

It ports the native Codex interrupt behavior from official CLIProxyAPI v8.0.23:

- `6331b38bba2cb716480fe78b6638b87db45e521d`: in-flight response.interrupt
  handling for serial and duplex WebSocket responses, including cancellation
  of HTTP-backed turns and same-socket continuation.
- `d318bcc3afb9ea8782862f5e8cdb541afdc9dfa5`: completed-response tracking,
  idempotent late interrupts, atomic connection state checks after credential
  validation, sanitized thread-safe diagnostics, and reader cleanup.
- `d31b61cb87c2664416368686c13f8bcd48e110a3`: prerequisite terminal upstream
  error tracking and propagation during WebSocket session activation.

Imports remain on the v7 module. The existing managed upgrade marker is retained
while applying the upstream handler changes. The upstream HTTP-cancel test is
synchronized with a completion channel instead of immediately inspecting an
atomic flag: downstream terminal-frame delivery is not a join barrier for the
mock executor. The test still requires actual context cancellation within two
seconds. No production cancellation behavior is changed from the upstream port.

Codex CLI 0.162.1, upstream source tag rust-v0.162.1 at
`092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`, sends response.interrupt with the
active response_id and mode=discard_partial_items when user steering preempts a
model request. It drains the terminal response and follows with response.create
using previous_response_id. The control frame is not response.steer and is not
the app-server question tool protocol.

Native interrupts are supported with the existing serial default; enabling
experimental codex.response-steering is not required. Active WebSocket control
frames are forwarded unchanged on the authenticated current socket. HTTP turns
are canceled and a terminal interrupted response is synthesized. Idle,
malformed, unknown-ID and disabled-account cases retain explicit validation;
known completed responses tolerate late interrupts without poisoning the next
turn. No retry, redial, credential substitution or model substitution is added.
