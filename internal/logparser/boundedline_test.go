package logparser

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadBoundedLine_NormalLine(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("hello\n"), 64)

	line, truncated, err := ReadBoundedLine(br, 1024)

	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, "hello", string(line))
}

func TestReadBoundedLine_MultipleLines(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("one\ntwo\nthree\n"), 64)

	for _, want := range []string{"one", "two", "three"} {
		line, truncated, err := ReadBoundedLine(br, 1024)
		require.NoError(t, err)
		assert.False(t, truncated)
		assert.Equal(t, want, string(line))
	}

	line, truncated, err := ReadBoundedLine(br, 1024)
	require.ErrorIs(t, err, io.EOF)
	assert.False(t, truncated)
	assert.Empty(t, line)
}

// TestReadBoundedLine_FinalLineNoNewline covers the case the patch claims to
// handle: the last line of input has no trailing '\n'. The reader returns the
// partial line together with io.EOF; callers must process the line before
// reacting to the error.
func TestReadBoundedLine_FinalLineNoNewline(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("trailing"), 64)

	line, truncated, err := ReadBoundedLine(br, 1024)

	require.ErrorIs(t, err, io.EOF)
	assert.False(t, truncated)
	assert.Equal(t, "trailing", string(line))
}

// TestReadBoundedLine_LineExactlyAtCap verifies the off-by-one boundary:
// a line of exactly cap bytes must NOT be reported as truncated.
func TestReadBoundedLine_LineExactlyAtCap(t *testing.T) {
	const lim = 1024

	payload := strings.Repeat("a", lim) + "\n"
	br := bufio.NewReaderSize(strings.NewReader(payload), 64)

	line, truncated, err := ReadBoundedLine(br, lim)

	require.NoError(t, err)
	assert.False(t, truncated, "exactly-at-cap line must not be flagged truncated")
	assert.Len(t, line, lim)
}

// TestReadBoundedLine_LineExceedsCap_DiscardsRemainder verifies the soft-cap
// discard path: a line longer than cap is truncated to cap bytes, the rest is
// silently consumed from the reader, and the next call sees the next line
// cleanly.
func TestReadBoundedLine_LineExceedsCap_DiscardsRemainder(t *testing.T) {
	const lim = 1024

	huge := strings.Repeat("x", lim*5)
	payload := huge + "\n" + "next\n"
	br := bufio.NewReaderSize(strings.NewReader(payload), 64)

	line, truncated, err := ReadBoundedLine(br, lim)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, line, lim)

	// The next call must see "next" cleanly, proving the excess was drained.
	line2, truncated2, err2 := ReadBoundedLine(br, lim)
	require.NoError(t, err2)
	assert.False(t, truncated2)
	assert.Equal(t, "next", string(line2))
}

// TestReadBoundedLine_PartialLineWithError covers the (n>0, err) io.Reader
// contract: a reader that returns bytes AND a non-EOF error on the same Read.
// The new partial-line handling must process the line and surface the error.
func TestReadBoundedLine_PartialLineWithError(t *testing.T) {
	sentinel := errors.New("synthetic io failure")
	r := &errAfterReader{data: []byte("partial-no-newline"), err: sentinel}
	br := bufio.NewReaderSize(r, 64)

	line, truncated, err := ReadBoundedLine(br, 1024)

	require.ErrorIs(t, err, sentinel)
	assert.False(t, truncated)
	assert.Equal(t, "partial-no-newline", string(line))
}

// TestReadBoundedLine_LineSpansInternalBuffer verifies that a line longer
// than the bufio.Reader's internal buffer (which triggers ErrBufferFull from
// ReadSlice internally) still assembles correctly when below the soft cap.
func TestReadBoundedLine_LineSpansInternalBuffer(t *testing.T) {
	// Internal bufio buffer = 64 bytes; line = 200 bytes. Forces multiple
	// ErrBufferFull iterations inside the helper.
	want := strings.Repeat("z", 200)
	br := bufio.NewReaderSize(strings.NewReader(want+"\n"), 64)

	line, truncated, err := ReadBoundedLine(br, 1024)

	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, want, string(line))
}

// TestReadBoundedLine_EmptyReader covers immediate EOF on an empty reader.
func TestReadBoundedLine_EmptyReader(t *testing.T) {
	br := bufio.NewReaderSize(bytes.NewReader(nil), 64)

	line, truncated, err := ReadBoundedLine(br, 1024)

	require.ErrorIs(t, err, io.EOF)
	assert.False(t, truncated)
	assert.Empty(t, line)
}

// TestReadBoundedLine_ExceedsCapAtEOF covers the case where an oversized line
// is followed by EOF without a trailing newline. The discard loop must
// terminate on EOF rather than spin forever.
func TestReadBoundedLine_ExceedsCapAtEOF(t *testing.T) {
	const lim = 64

	payload := strings.Repeat("y", lim*4) // no trailing '\n'
	br := bufio.NewReaderSize(strings.NewReader(payload), 32)

	line, truncated, err := ReadBoundedLine(br, lim)

	require.ErrorIs(t, err, io.EOF)
	assert.True(t, truncated)
	assert.Len(t, line, lim)
}
