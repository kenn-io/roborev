//go:build linux || integration

package main

import (
	"bytes"
	"sync"
)

// syncBuffer is a goroutine-safe bytes.Buffer for capturing subprocess
// output: the exec stdout-copy goroutine writes while assertion message
// arguments read, and those arguments are evaluated even on success.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
