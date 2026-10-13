package executor

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeDateCheckpointSameFileReplacementAndTokenRefresh(t *testing.T) {
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)
	auth := &cliproxyauth.Auth{ID: "same-auth-file", Provider: "claude", Metadata: map[string]any{
		"account_uuid": "11111111-2222-4333-8444-555555555555", "access_token": "synthetic-old-token", "email": "synthetic@example.invalid",
	}}
	ctx := helps.WithClaudeSessionID(context.Background(), "synthetic-date-session")
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"synthetic fixture"}]}`)
	_, _, before, ok := resolveClaudeContinuityTags(ctx, nil, auth, http.Header{}, payload, false, "", "")
	if !ok {
		t.Fatal("native continuity missing")
	}
	snapshot, err := helps.SnapshotClaudeSessionDates(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pins) != 1 {
		t.Fatal("date pin was not captured")
	}
	// Make the old pin differ from the current date so the resumed wire fixture
	// proves it was restored rather than silently reconstructed from the clock.
	snapshot.Pins[0].Date = "2000-01-01"
	helps.ResetClaudeDiagnosticsForTest()
	if err := helps.RestoreClaudeSessionDates(snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	refreshed := auth.Clone()
	refreshed.Metadata["access_token"] = "synthetic-refreshed-token"
	refreshed.Metadata["refresh_token"] = "synthetic-refreshed-refresh-token"
	_, _, resumed, ok := resolveClaudeContinuityTags(ctx, nil, refreshed, http.Header{}, payload, false, "", "")
	if !ok || resumed.Key != before.Key || resumed.PinnedDate != "2000-01-01" {
		t.Fatal("token refresh changed restored native date identity")
	}
	out := checkSystemInstructionsWithSigningModeAt(payload, false, false, "2.1.280", "cli", "", resumed.PinnedDate, false, "", "", false)
	if extractClaudeCloakDate(t, out) != "2000-01-01" {
		t.Fatal("restored date did not reach rendered cloak prefix")
	}
	replacement := auth.Clone()
	replacement.Metadata["account_uuid"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	_, _, replaced, ok := resolveClaudeContinuityTags(ctx, nil, replacement, http.Header{}, payload, false, "", "")
	if !ok || replaced.Key == before.Key || replaced.PinnedDate == "2000-01-01" {
		t.Fatal("same-file account replacement reused restored date")
	}
}
