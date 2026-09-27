package snapshotstore

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"
)

type countingReader struct {
	remaining int64
	read      int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if int64(count) > r.remaining {
		count = int(r.remaining)
	}
	for i := 0; i < count; i++ {
		buffer[i] = 'x'
	}
	r.remaining -= int64(count)
	r.read += int64(count)
	return count, nil
}

func TestBoundedRangeStopsReadingAnOversizedLineAtBudget(t *testing.T) {
	reader := &countingReader{remaining: 100 << 20}
	const maxBytes = 24 * 1024
	_, _, _, _, _, err := readBoundedLines(context.Background(), bufio.NewReaderSize(reader, 4096), 1, 1, maxBytes, true)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("oversized first line error = %v, want ErrLineTooLong", err)
	}
	if reader.read > maxBytes+8192 {
		t.Fatalf("bounded reader consumed %d bytes for a %d byte budget", reader.read, maxBytes)
	}
	if reader.read >= 100<<20 {
		t.Fatalf("bounded reader consumed the full sparse input: %d bytes", reader.read)
	}
}
