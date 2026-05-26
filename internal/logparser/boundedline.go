package logparser

import (
	"bufio"
	"errors"
)

// MaxLineBytes is the soft cap applied to a single log line. Real Opus
// thinking blocks routinely run a few MiB once JSON-encoded; 16 MiB gives
// roughly 5x headroom over realistic worst-case while bounding memory
// pressure when a wedged worker emits unbounded text without a newline.
//
// On overflow, ReadBoundedLine drops the rest of the line and returns
// truncated=true so the caller can decide how to surface the gap.
const MaxLineBytes = 16 * 1024 * 1024

// ReadBoundedLine reads a line from r terminated by '\n', accumulating up to
// maxBytes of *content* (excluding the trailing newline) into the returned
// slice. If the line's content is longer than maxBytes, the returned slice
// contains the first maxBytes, truncated is true, and the rest of the line up
// to and including the next '\n' is silently consumed from r so the next call
// starts on a fresh line.
//
// The trailing '\n' is never included in the returned slice. A trailing '\r'
// is left in place — callers that want CRLF handling should TrimRight it.
//
// On underlying read error or EOF, returns (line read so far, truncated,
// err). Callers should process the partial line before reacting to the
// error, mirroring bufio.Reader.ReadString semantics.
func ReadBoundedLine(r *bufio.Reader, maxBytes int) (line []byte, truncated bool, err error) {
	for {
		// ReadSlice returns chunks bounded by the reader's internal buffer
		// (we set it to 64 KiB at the call site). ErrBufferFull means the
		// line continued past the buffer; loop and accumulate.
		chunk, e := r.ReadSlice('\n')

		// Strip the trailing '\n' from the count when the delimiter is
		// in this chunk. Otherwise (ErrBufferFull / EOF / I/O error)
		// the chunk has no delimiter and is all content.
		contentLen := len(chunk)
		if e == nil && contentLen > 0 && chunk[contentLen-1] == '\n' {
			contentLen--
		}

		if !truncated {
			avail := maxBytes - len(line)
			if contentLen > avail {
				if avail > 0 {
					line = append(line, chunk[:avail]...)
				}

				truncated = true
			} else {
				line = append(line, chunk[:contentLen]...)
			}
		}

		switch {
		case e == nil:
			return line, truncated, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		default:
			return line, truncated, e
		}
	}
}
