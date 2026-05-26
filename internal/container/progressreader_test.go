package container

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProgressReader_BumpsOnPositiveRead verifies that onProgress fires
// once per Read call that returned at least one byte, even if the same
// Read also surfaced an error. This is the (n>0, err) io.Reader contract
// the idle watchdog wrapper depends on.
func TestProgressReader_BumpsOnPositiveRead(t *testing.T) {
	var ticks int

	pr := &progressReader{
		r:          bytes.NewReader([]byte("hello")),
		onProgress: func() { ticks++ },
	}

	buf := make([]byte, 8)

	n, err := pr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 1, ticks, "onProgress must fire once for the 5-byte read")

	n, err = pr.Read(buf)
	assert.Equal(t, 0, n)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 1, ticks, "onProgress must NOT fire on a zero-byte EOF read")
}

// TestProgressReader_BumpsOnPartialReadWithError covers the legal io.Reader
// contract where a Read returns (n>0, err) — onProgress must still fire so
// the watchdog notices the bytes that arrived alongside the error.
func TestProgressReader_BumpsOnPartialReadWithError(t *testing.T) {
	var ticks int

	sentinel := errors.New("synthetic")
	pr := &progressReader{
		r:          &errAfterReader{data: []byte("partial"), err: sentinel},
		onProgress: func() { ticks++ },
	}

	buf := make([]byte, 32)

	n, err := pr.Read(buf)
	require.NoError(t, err, "errAfterReader returns nil err while data remains")
	assert.Equal(t, 7, n)
	assert.Equal(t, 1, ticks)

	n, err = pr.Read(buf)
	assert.Equal(t, 0, n)
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, ticks, "onProgress must NOT fire on a zero-byte error read")
}

// TestProgressReader_NilCallback verifies the wrapper does not panic when
// onProgress is nil — defensive against accidental misuse.
func TestProgressReader_NilCallback(t *testing.T) {
	pr := &progressReader{r: bytes.NewReader([]byte("ok")), onProgress: nil}

	buf := make([]byte, 8)
	n, err := pr.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// errAfterReader yields data once, then returns err on subsequent reads.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]

		return n, nil
	}

	return 0, r.err
}
