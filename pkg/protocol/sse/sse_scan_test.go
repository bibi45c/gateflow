package sse

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"
)

// gatewayReads is how many reads of readSize bytes fill 1 MiB, the gateway's limit
// on one event.
const gatewayReads = (1 << 20) / readSize

// nextBlock follows the long block in every stream from longLineStream.
const nextBlock = "data: next\n\n"

// longLineStream returns a block whose one data: line holds size bytes, ended by
// ending twice, and then nextBlock, so that the long block's Raw boundary is checked.
func longLineStream(size int, ending string) (stream, data []byte) {
	data = make([]byte, size)
	for i := range data {
		data[i] = 'a' + byte(i%26)
	}
	stream = append([]byte("data: "), data...)
	stream = append(stream, ending+ending+nextBlock...)
	return stream, data
}

// checkLongLine reads stream in reads of at most chunk bytes and checks that it
// holds the long block from longLineStream and then nextBlock.
func checkLongLine(t *testing.T, stream, data []byte, chunk int) {
	t.Helper()
	events := readAll(t, &chunkReader{data: stream, size: chunk})
	if len(events) != 2 {
		t.Fatalf("got %d blocks, want 2", len(events))
	}
	if !bytes.Equal(events[0].Raw, stream[:len(stream)-len(nextBlock)]) {
		t.Errorf("long block: Raw of %d bytes differs from its %d bytes in the stream",
			len(events[0].Raw), len(stream)-len(nextBlock))
	}
	if !bytes.Equal(events[0].Data, data) || events[0].Name != "" {
		t.Errorf("long block: Name %q and %d bytes of data differ", events[0].Name, len(events[0].Data))
	}
	if string(events[1].Raw) != nextBlock || string(events[1].Data) != "next" {
		t.Errorf("next block: Raw %q, Data %q", events[1].Raw, events[1].Data)
	}
}

// countSearches makes the Reader's line-end searches add the bytes they examine to
// *n until the test ends.
func countSearches(t *testing.T, n *int) {
	t.Helper()
	search := indexLineEnd
	indexLineEnd = func(b []byte) int {
		i := search(b)
		if i < 0 {
			*n += len(b)
		} else {
			*n += i + 1
		}
		return i
	}
	t.Cleanup(func() { indexLineEnd = search })
}

func TestReaderReadsLongLineSplitAcrossReads(t *testing.T) {
	for _, ending := range []struct{ label, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		for _, size := range []int{3*readSize + 5, 1 << 20} {
			for _, chunk := range []int{1000, readSize} {
				name := ending.label + "/" + strconv.Itoa(size) + "/" + strconv.Itoa(chunk)
				t.Run(name, func(t *testing.T) {
					stream, data := longLineStream(size, ending.value)
					checkLongLine(t, stream, data, chunk)
				})
			}
		}
	}
}

func TestReaderReadsLongLineWithCRAtTheEndOfARead(t *testing.T) {
	split := gatewayReads * readSize // where one read ends and the next starts
	cases := []struct {
		label, ending string
		size          int
	}{
		{"CRLF line end", "\r\n", split - len("data: ") - 1},
		{"CRLF blank line", "\r\n", split - len("data: ") - 3},
		{"CR line end", "\r", split - len("data: ") - 1},
		{"CR blank line", "\r", split - len("data: ") - 2},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			stream, data := longLineStream(c.size, c.ending)
			if stream[split-1] != '\r' || (c.ending == "\r\n" && stream[split] != '\n') {
				t.Fatalf("the read ends at %q, not at a \"\\r\" before the rest of its line ending", stream[split-1:split+1])
			}
			checkLongLine(t, stream, data, readSize)
		})
	}
}

// TestReaderSearchesEachByteOnce counts the bytes the Reader examines for line
// endings rather than timing it: each byte once, and a "\r" that ends a read once
// more, keeps the count within twice the stream.
func TestReaderSearchesEachByteOnce(t *testing.T) {
	var searched int
	countSearches(t, &searched)
	for _, reads := range []int{16, 64, gatewayReads} {
		// The long line's "\r\n" is split across the last two reads.
		stream, data := longLineStream(reads*readSize-len("data: ")-1, "\r\n")
		searched = 0
		checkLongLine(t, stream, data, readSize)
		t.Logf("%d bytes in reads of %d: searched %d bytes, %.2f per byte",
			len(stream), readSize, searched, float64(searched)/float64(len(stream)))
		if searched > 2*len(stream) {
			t.Errorf("%d bytes: searched %d bytes, want at most %d", len(stream), searched, 2*len(stream))
		}
	}
}

// BenchmarkReaderLongLine reads one long data: line in reads of readSize bytes. Its
// time per byte should stay about the same across sizes; compare by hand with
// go test -run '^$' -bench . -benchtime 5x.
func BenchmarkReaderLongLine(b *testing.B) {
	for _, size := range []struct {
		label string
		bytes int
	}{{"64KiB", 64 << 10}, {"256KiB", 256 << 10}, {"1MiB", 1 << 20}} {
		stream, _ := longLineStream(size.bytes, "\n")
		b.Run(size.label, func(b *testing.B) {
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				reader := NewReader(&chunkReader{data: stream, size: readSize})
				for {
					_, err := reader.Next()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
