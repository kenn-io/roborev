package daemon

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkOutputWriterLineLength(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20, 16 << 20} {
		for _, shortLines := range []bool{false, true} {
			name := "unterminated"
			if shortLines {
				name = "short"
			}
			b.Run(fmt.Sprintf("%s/%dMiB", name, size>>20), func(b *testing.B) {
				data := bytes.Repeat([]byte("x"), size)
				if shortLines {
					for i := 69; i < len(data); i += 70 {
						data[i] = '\n'
					}
				}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					ob := NewOutputBuffer(size, size)
					w := ob.Writer(1, simpleNormalizer)
					for offset := 0; offset < len(data); offset += 4096 {
						_, _ = w.Write(data[offset:min(offset+4096, len(data))])
					}
					w.Flush()
				}
			})
		}
	}
}
