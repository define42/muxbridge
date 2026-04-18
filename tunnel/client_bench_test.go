package tunnel

import (
	"bytes"
	"context"
	"testing"
)

func BenchmarkDispatchWSInbound(b *testing.B) {
	ctx := context.Background()
	ch := make(chan []byte, 1)
	payload := bytes.Repeat([]byte("x"), 32*1024)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := dispatchWSInbound(ctx, ch, payload); err != nil {
			b.Fatalf("dispatchWSInbound error: %v", err)
		}
		<-ch
	}
}
