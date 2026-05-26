package container

import "io"

// progressReader wraps an io.Reader and invokes onProgress whenever the
// underlying Read returns at least one byte. Used to feed the idle
// watchdog from raw byte arrival rather than completed-line emission so
// a slow large line does not appear idle to the watchdog.
type progressReader struct {
	r          io.Reader
	onProgress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onProgress != nil {
		p.onProgress()
	}

	return n, err
}
