package fixture

import "io"

// Reader defines basic read behavior.
type Reader interface {
	Read(p []byte) (n int, err error)
}

// ReadCloser embeds Reader and io.Closer.
type ReadCloser interface {
	Reader
	io.Closer
}

// Processor defines data processing interface.
type Processor interface {
	Process(data []byte) ([]byte, error)
}
