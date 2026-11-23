package openai

import (
	"net/http"

	"github.com/MoeForge/moego/ojson"
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

func (h *OpenAIResponsesAPIHandler) writeInitialResponsesError(c *gin.Context, msg *interfaces.ErrorMessage) {
	if !c.Writer.Written() {
		msg = nativeResponsesPoolQuotaError(msg)
	}
	h.WriteErrorResponse(c, msg)
}

// Only the manager can certify exhaustion of the full eligible route pool.
// A selector's cooldown error may instead describe a pinned or tried subset.
func nativeResponsesPoolQuotaError(msg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if msg == nil || msg.DirectResponse || msg.StatusCode != http.StatusTooManyRequests ||
		clienterror.HTTPStatusFromError(msg.Error) != http.StatusTooManyRequests {
		return msg
	}
	_, exhausted := coreauth.PoolQuotaExhaustionDetails(msg.Error)
	if !exhausted {
		return msg
	}
	// Native Codex recognizes this type as terminal rather than sleeping Retry-After.
	// Do not expose local admission estimates as provider reset metadata.
	doc := ojson.NewDoc([]byte(`{}`))
	err := doc.Set("error", map[string]any{
		"type":      "usage_limit_reached",
		"code":      "model_cooldown",
		"message":   "All eligible credentials for the requested model are quota limited.",
		"retryable": false,
	})
	if err != nil {
		return msg
	}
	terminal := *msg
	terminal.Error = &responsesPoolQuotaError{body: doc.String(), cause: msg.Error}
	return &terminal
}
