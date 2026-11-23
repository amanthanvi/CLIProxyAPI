package responses

import (
	"bytes"
	"context"
	"testing"
)

func TestNativeCodexAsyncQuestionMetadataPreserved(t *testing.T) {
	const item = `{"id":"msg_async_question","type":"message","role":"assistant","status":"completed","delivery":"async","phase":"commentary","questions":[{"title":"Preferred color?","options":["Green","Red"]}],"content":[{"type":"output_text","text":"Preferred color?"}]}`
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[` + item + `]}}`,
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","output":[` + item + `]}}`,
	} {
		for _, prefix := range []string{"", "data: "} {
			raw := []byte(prefix + event)
			var param any
			outputs := ConvertCodexResponseToOpenAIResponses(context.Background(), "fixture-model", nil, nil, raw, &param)
			if len(outputs) != 1 || !bytes.Equal(outputs[0], raw) {
				t.Fatalf("native question metadata changed: input=%s output=%s", raw, outputs)
			}
		}
	}
	for _, status := range []string{"completed", "incomplete"} {
		response := `{"id":"r1","status":"` + status + `","output":[` + item + `]}`
		var param any
		got := ConvertCodexResponseToOpenAIResponsesNonStream(context.Background(), "fixture-model", nil, nil, []byte(`{"type":"response.`+status+`","response":`+response+`}`), &param)
		if !bytes.Equal(got, []byte(response)) {
			t.Fatalf("nonstream native question metadata changed: %s", got)
		}
	}
}
