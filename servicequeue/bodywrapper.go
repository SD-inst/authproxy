package servicequeue

import "io"

type BodyWrapper struct {
	io.ReadCloser
	onClose func()
}

func (b BodyWrapper) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return b.ReadCloser.Close()
}
