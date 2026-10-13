package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type recordingStreamingChunks struct {
	testStreamingLogWriter
	chunks [][]byte
}

func (w *recordingStreamingChunks) WriteChunkAsync(chunk []byte) {
	w.chunks = append(w.chunks, bytes.Clone(chunk))
}

func TestStreamingChunkProcessorStartsAfterChannelReleased(t *testing.T) {
	writer := &recordingStreamingChunks{}
	chunks := make(chan []byte, 2)
	chunks <- []byte("data: first\n\n")
	chunks <- []byte("data: second\n\n")
	wrapper := &ResponseWriterWrapper{streamWriter: writer, chunkChannel: chunks}
	// Force the normal finalization ordering before the goroutine is scheduled.
	close(wrapper.chunkChannel)
	wrapper.chunkChannel = nil
	done := make(chan struct{})
	processStreamingChunks(writer, chunks, done)
	<-done
	if len(writer.chunks) != 2 || !bytes.Equal(writer.chunks[0], []byte("data: first\n\n")) || !bytes.Equal(writer.chunks[1], []byte("data: second\n\n")) {
		t.Fatal("late chunk processor discarded queued streaming log chunks")
	}
}

func TestStreamingFinalizeDrainsQueuedChunksBeforeClosingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	writer := &recordingStreamingChunks{}
	chunks := make(chan []byte, 2)
	chunks <- []byte("data: first\n\n")
	chunks <- []byte("data: second\n\n")
	done := make(chan struct{})
	wrapper := &ResponseWriterWrapper{
		ResponseWriter: c.Writer, logger: &testRequestLogger{enabled: true},
		requestInfo: &RequestInfo{}, statusCode: http.StatusOK,
		isStreaming: true, streamWriter: writer, chunkChannel: chunks, streamDone: done,
	}
	go processStreamingChunks(writer, chunks, done)
	if err := wrapper.Finalize(c); err != nil {
		t.Fatal(err)
	}
	if len(writer.chunks) != 2 || !writer.closed {
		t.Fatal("finalization did not drain queued chunks before closing the writer")
	}
	if wrapper.chunkChannel != nil || wrapper.streamDone != nil || wrapper.streamWriter != nil {
		t.Fatal("finalization did not release streaming resources")
	}
}
