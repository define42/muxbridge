package server

import (
	"bytes"
	"testing"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func BenchmarkHandleClientFrameResponseBody(b *testing.B) {
	srv := &Server{}
	state := newResponseState()
	sess := &session{
		inflight: map[string]*responseState{
			"req-1": state,
		},
	}
	frame := &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_ResponseBody{ResponseBody: &tunnelpb.ResponseBody{
			RequestId: "req-1",
			Chunk:     bytes.Repeat([]byte("x"), 32*1024),
		}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv.handleClientFrame(sess, frame)
		<-state.body
	}
}

func BenchmarkHandleClientFrameWsData(b *testing.B) {
	srv := &Server{}
	ws := &wsState{
		fromClient: make(chan []byte, 1),
		done:       make(chan struct{}),
	}
	sess := &session{
		wsMap: map[string]*wsState{
			"ws-1": ws,
		},
	}
	frame := &tunnelpb.ClientFrame{
		Msg: &tunnelpb.ClientFrame_WsData{WsData: &tunnelpb.WebSocketData{
			RequestId: "ws-1",
			Payload:   bytes.Repeat([]byte("x"), 32*1024),
		}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv.handleClientFrame(sess, frame)
		<-ws.fromClient
	}
}
