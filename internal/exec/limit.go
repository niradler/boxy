package exec

import (
	"bytes"
	"fmt"
)

type LimitedWriter struct {
	buf *bytes.Buffer
	max int
	hit bool
}

func NewLimitedWriter(max int) *LimitedWriter {
	return &LimitedWriter{buf: &bytes.Buffer{}, max: max}
}

func (l *LimitedWriter) Write(p []byte) (int, error) {
	if l.max <= 0 {
		return l.buf.Write(p)
	}
	remain := l.max - l.buf.Len()
	if remain <= 0 {
		l.hit = true
		return 0, fmt.Errorf("output limit exceeded")
	}
	if len(p) > remain {
		_, _ = l.buf.Write(p[:remain])
		l.hit = true
		return remain, fmt.Errorf("output limit exceeded")
	}
	return l.buf.Write(p)
}

func (l *LimitedWriter) Bytes() []byte {
	if l.buf == nil {
		return nil
	}
	return l.buf.Bytes()
}

func (l *LimitedWriter) HitLimit() bool {
	return l.hit
}
