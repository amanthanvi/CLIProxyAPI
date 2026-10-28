package openai

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type responsesPoolQuotaError struct {
	body  string
	cause error
}

func (e *responsesPoolQuotaError) Error() string { return e.body }
func (e *responsesPoolQuotaError) Unwrap() error { return e.cause }

// Only the manager can certify exhaustion of the full eligible route pool.
// A selector's cooldown error may instead describe a pinned or tried subset.
func (h *OpenAIResponsesAPIHandler) writeInitialResponsesError(c *gin.Context, msg *interfaces.ErrorMessage) {
	if msg == nil || msg.DirectResponse || c.Writer.Written() || msg.StatusCode != http.StatusTooManyRequests ||
		clienterror.HTTPStatusFromError(msg.Error) != http.StatusTooManyRequests {
		h.WriteErrorResponse(c, msg)
		return
	}
	_, exhausted := coreauth.PoolQuotaExhaustionDetails(msg.Error)
	if !exhausted {
		h.WriteErrorResponse(c, msg)
		return
	}
	// Native Codex recognizes this type as terminal rather than sleeping Retry-After.
	// Do not expose local admission estimates as provider reset metadata.
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type":      "usage_limit_reached",
		"code":      "model_cooldown",
		"message":   "All eligible credentials for the requested model are quota limited.",
		"retryable": false,
	}})
	if err != nil {
		h.WriteErrorResponse(c, msg)
		return
	}
	terminal := *msg
	terminal.Error = &responsesPoolQuotaError{body: string(body), cause: msg.Error}
	h.WriteErrorResponse(c, &terminal)
}
