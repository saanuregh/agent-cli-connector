package pi

import (
	"io"
	"strings"
	"testing"
)

func TestLastByteReadsThroughAReaderWithoutOffsets(t *testing.T) {
	r := struct{ io.Reader }{strings.NewReader("a\nend")}
	if b, err := lastByte(r, 5); err != nil || b != 'd' {
		t.Errorf("lastByte = %q, %v; want 'd'", b, err)
	}
	if _, err := lastByte(struct{ io.Reader }{strings.NewReader("ab")}, 5); err == nil {
		t.Error("lastByte of a file shorter than its size succeeded")
	}
}
