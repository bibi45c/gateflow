package anthropic

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// TestReaderReadsLongEventAcrossReads reads a content_block_delta of nearly 1 MiB,
// the gateway's limit on one event, through this package's Reader in small reads.
// The scanning itself is tested in pkg/protocol/sse.
func TestReaderReadsLongEventAcrossReads(t *testing.T) {
	text := strings.Repeat("0123456789", (1<<20)/10-16)
	data := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`
	long := "event: content_block_delta\r\ndata: " + data + "\r\n\r\n"
	stream := []byte(long + "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n")
	for _, chunk := range []int{1000, 4096} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			events := readAll(t, &chunkReader{data: stream, size: chunk})
			if len(events) != 2 || !bytes.Equal(joinRaw(events), stream) {
				t.Fatalf("got %d blocks; their Raw joined differ from the stream", len(events))
			}
			if string(events[0].Raw) != long || string(events[0].Data) != data {
				t.Errorf("long event: Raw of %d bytes, data of %d bytes", len(events[0].Raw), len(events[0].Data))
			}
			if events[0].Type() != EventContentBlockDelta || events[1].Type() != EventMessageStop {
				t.Errorf("types %q and %q", events[0].Type(), events[1].Type())
			}
		})
	}
}
