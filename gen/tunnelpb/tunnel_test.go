package tunnelpb

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type protoTestService struct {
	UnimplementedTunnelServiceServer
}

func (protoTestService) Connect(stream TunnelService_ConnectServer) error {
	frame, err := stream.Recv()
	if err != nil {
		return err
	}
	if frame.GetRegister() == nil || frame.GetRegister().GetToken() != "demo-token" {
		return status.Error(codes.InvalidArgument, "missing register")
	}
	return stream.Send(&ServerFrame{
		Msg: &ServerFrame_Ping{Ping: &Ping{UnixNano: 99}},
	})
}

func callNoArg(fn func()) {
	fn()
}

func TestMessagesAndFrames(t *testing.T) {
	t.Parallel()

	header := &Header{Key: "X-Test", Values: []string{"one", "two"}}
	_ = header.String()
	callNoArg(header.ProtoMessage)
	if got := header.ProtoReflect().Descriptor().FullName(); got != "tunnel.v1.Header" {
		t.Fatalf("Header full name = %q, want %q", got, "tunnel.v1.Header")
	}
	if desc, idx := header.Descriptor(); len(desc) == 0 || len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("Header descriptor = %d bytes, idx=%v", len(desc), idx)
	}
	if header.GetKey() != "X-Test" || len(header.GetValues()) != 2 {
		t.Fatalf("Header getters returned unexpected values: %#v", header)
	}
	header.Reset()

	register := &Register{TunnelId: "demo.example.com", Token: "demo-token"}
	_ = register.String()
	callNoArg(register.ProtoMessage)
	_ = register.ProtoReflect()
	_, _ = register.Descriptor()
	if register.GetTunnelId() != "demo.example.com" || register.GetToken() != "demo-token" {
		t.Fatalf("Register getters returned unexpected values: %#v", register)
	}
	register.Reset()

	ping := &Ping{UnixNano: 10}
	_ = ping.String()
	callNoArg(ping.ProtoMessage)
	_ = ping.ProtoReflect()
	_, _ = ping.Descriptor()
	if ping.GetUnixNano() != 10 {
		t.Fatalf("Ping unix_nano = %d, want 10", ping.GetUnixNano())
	}
	ping.Reset()

	pong := &Pong{UnixNano: 11}
	_ = pong.String()
	callNoArg(pong.ProtoMessage)
	_ = pong.ProtoReflect()
	_, _ = pong.Descriptor()
	if pong.GetUnixNano() != 11 {
		t.Fatalf("Pong unix_nano = %d, want 11", pong.GetUnixNano())
	}
	pong.Reset()

	requestStart := &RequestStart{
		RequestId:  "req-1",
		Method:     "GET",
		Scheme:     "https",
		Host:       "demo.example.com",
		Path:       "/",
		RawPath:    "/",
		RawQuery:   "x=1",
		Headers:    []*Header{{Key: "X-Test", Values: []string{"one"}}},
		RemoteAddr: "127.0.0.1:1234",
	}
	_ = requestStart.String()
	callNoArg(requestStart.ProtoMessage)
	_ = requestStart.ProtoReflect()
	_, _ = requestStart.Descriptor()
	if requestStart.GetRequestId() != "req-1" || requestStart.GetMethod() != "GET" || requestStart.GetScheme() != "https" || requestStart.GetHost() != "demo.example.com" || requestStart.GetPath() != "/" || requestStart.GetRawPath() != "/" || requestStart.GetRawQuery() != "x=1" || requestStart.GetRemoteAddr() != "127.0.0.1:1234" || len(requestStart.GetHeaders()) != 1 {
		t.Fatalf("RequestStart getters returned unexpected values: %#v", requestStart)
	}
	requestStart.Reset()

	requestBody := &RequestBody{RequestId: "req-1", Chunk: []byte("hello")}
	_ = requestBody.String()
	callNoArg(requestBody.ProtoMessage)
	_ = requestBody.ProtoReflect()
	_, _ = requestBody.Descriptor()
	if requestBody.GetRequestId() != "req-1" || string(requestBody.GetChunk()) != "hello" {
		t.Fatalf("RequestBody getters returned unexpected values: %#v", requestBody)
	}
	requestBody.Reset()

	requestEnd := &RequestEnd{RequestId: "req-1"}
	_ = requestEnd.String()
	callNoArg(requestEnd.ProtoMessage)
	_ = requestEnd.ProtoReflect()
	_, _ = requestEnd.Descriptor()
	if requestEnd.GetRequestId() != "req-1" {
		t.Fatalf("RequestEnd request_id = %q, want %q", requestEnd.GetRequestId(), "req-1")
	}
	requestEnd.Reset()

	cancelRequest := &CancelRequest{RequestId: "req-2"}
	_ = cancelRequest.String()
	callNoArg(cancelRequest.ProtoMessage)
	_ = cancelRequest.ProtoReflect()
	_, _ = cancelRequest.Descriptor()
	if cancelRequest.GetRequestId() != "req-2" {
		t.Fatalf("CancelRequest request_id = %q, want %q", cancelRequest.GetRequestId(), "req-2")
	}
	cancelRequest.Reset()

	wsData := &WebSocketData{RequestId: "ws-1", Payload: []byte("hello")}
	_ = wsData.String()
	callNoArg(wsData.ProtoMessage)
	_ = wsData.ProtoReflect()
	_, _ = wsData.Descriptor()
	if wsData.GetRequestId() != "ws-1" || string(wsData.GetPayload()) != "hello" {
		t.Fatalf("WebSocketData getters returned unexpected values: %#v", wsData)
	}
	wsData.Reset()

	wsClose := &WebSocketClose{RequestId: "ws-1"}
	_ = wsClose.String()
	callNoArg(wsClose.ProtoMessage)
	_ = wsClose.ProtoReflect()
	_, _ = wsClose.Descriptor()
	if wsClose.GetRequestId() != "ws-1" {
		t.Fatalf("WebSocketClose request_id = %q, want %q", wsClose.GetRequestId(), "ws-1")
	}
	wsClose.Reset()

	responseStart := &ResponseStart{RequestId: "req-1", StatusCode: 201, Headers: []*Header{{Key: "X-Reply", Values: []string{"ok"}}}}
	_ = responseStart.String()
	callNoArg(responseStart.ProtoMessage)
	_ = responseStart.ProtoReflect()
	_, _ = responseStart.Descriptor()
	if responseStart.GetRequestId() != "req-1" || responseStart.GetStatusCode() != 201 || len(responseStart.GetHeaders()) != 1 {
		t.Fatalf("ResponseStart getters returned unexpected values: %#v", responseStart)
	}
	responseStart.Reset()

	responseBody := &ResponseBody{RequestId: "req-1", Chunk: []byte("world")}
	_ = responseBody.String()
	callNoArg(responseBody.ProtoMessage)
	_ = responseBody.ProtoReflect()
	_, _ = responseBody.Descriptor()
	if responseBody.GetRequestId() != "req-1" || string(responseBody.GetChunk()) != "world" {
		t.Fatalf("ResponseBody getters returned unexpected values: %#v", responseBody)
	}
	responseBody.Reset()

	responseEnd := &ResponseEnd{RequestId: "req-1"}
	_ = responseEnd.String()
	callNoArg(responseEnd.ProtoMessage)
	_ = responseEnd.ProtoReflect()
	_, _ = responseEnd.Descriptor()
	if responseEnd.GetRequestId() != "req-1" {
		t.Fatalf("ResponseEnd request_id = %q, want %q", responseEnd.GetRequestId(), "req-1")
	}
	responseEnd.Reset()

	responseError := &ResponseError{RequestId: "req-1", Message: "boom"}
	_ = responseError.String()
	callNoArg(responseError.ProtoMessage)
	_ = responseError.ProtoReflect()
	_, _ = responseError.Descriptor()
	if responseError.GetRequestId() != "req-1" || responseError.GetMessage() != "boom" {
		t.Fatalf("ResponseError getters returned unexpected values: %#v", responseError)
	}
	responseError.Reset()

	clientFrame := &ClientFrame{Msg: &ClientFrame_Register{Register: &Register{Token: "demo-token"}}}
	_ = clientFrame.String()
	callNoArg(clientFrame.ProtoMessage)
	_ = clientFrame.ProtoReflect()
	_, _ = clientFrame.Descriptor()
	if clientFrame.GetMsg() == nil || clientFrame.GetRegister().GetToken() != "demo-token" || clientFrame.GetPong() != nil || clientFrame.GetResponseStart() != nil || clientFrame.GetResponseBody() != nil || clientFrame.GetResponseEnd() != nil || clientFrame.GetResponseError() != nil || clientFrame.GetWsData() != nil || clientFrame.GetWsClose() != nil {
		t.Fatalf("ClientFrame getters returned unexpected values: %#v", clientFrame)
	}
	clientFrame.Reset()

	clientFrame = &ClientFrame{Msg: &ClientFrame_Pong{Pong: &Pong{UnixNano: 12}}}
	if clientFrame.GetPong().GetUnixNano() != 12 {
		t.Fatalf("ClientFrame pong = %d, want 12", clientFrame.GetPong().GetUnixNano())
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_ResponseStart{ResponseStart: &ResponseStart{RequestId: "req"}}}
	if clientFrame.GetResponseStart().GetRequestId() != "req" {
		t.Fatalf("ClientFrame response start request_id = %q, want %q", clientFrame.GetResponseStart().GetRequestId(), "req")
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_ResponseBody{ResponseBody: &ResponseBody{RequestId: "req"}}}
	if clientFrame.GetResponseBody().GetRequestId() != "req" {
		t.Fatalf("ClientFrame response body request_id = %q, want %q", clientFrame.GetResponseBody().GetRequestId(), "req")
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_ResponseEnd{ResponseEnd: &ResponseEnd{RequestId: "req"}}}
	if clientFrame.GetResponseEnd().GetRequestId() != "req" {
		t.Fatalf("ClientFrame response end request_id = %q, want %q", clientFrame.GetResponseEnd().GetRequestId(), "req")
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_ResponseError{ResponseError: &ResponseError{RequestId: "req"}}}
	if clientFrame.GetResponseError().GetRequestId() != "req" {
		t.Fatalf("ClientFrame response error request_id = %q, want %q", clientFrame.GetResponseError().GetRequestId(), "req")
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_WsData{WsData: &WebSocketData{RequestId: "ws-1"}}}
	if clientFrame.GetWsData().GetRequestId() != "ws-1" {
		t.Fatalf("ClientFrame ws data request_id = %q, want %q", clientFrame.GetWsData().GetRequestId(), "ws-1")
	}
	clientFrame = &ClientFrame{Msg: &ClientFrame_WsClose{WsClose: &WebSocketClose{RequestId: "ws-1"}}}
	if clientFrame.GetWsClose().GetRequestId() != "ws-1" {
		t.Fatalf("ClientFrame ws close request_id = %q, want %q", clientFrame.GetWsClose().GetRequestId(), "ws-1")
	}

	serverFrame := &ServerFrame{Msg: &ServerFrame_RequestStart{RequestStart: &RequestStart{RequestId: "req"}}}
	_ = serverFrame.String()
	callNoArg(serverFrame.ProtoMessage)
	_ = serverFrame.ProtoReflect()
	_, _ = serverFrame.Descriptor()
	if serverFrame.GetMsg() == nil || serverFrame.GetRequestStart().GetRequestId() != "req" || serverFrame.GetRequestBody() != nil || serverFrame.GetRequestEnd() != nil || serverFrame.GetCancelRequest() != nil || serverFrame.GetPing() != nil || serverFrame.GetWsData() != nil || serverFrame.GetWsClose() != nil {
		t.Fatalf("ServerFrame getters returned unexpected values: %#v", serverFrame)
	}
	serverFrame.Reset()

	serverFrame = &ServerFrame{Msg: &ServerFrame_RequestBody{RequestBody: &RequestBody{RequestId: "req"}}}
	if serverFrame.GetRequestBody().GetRequestId() != "req" {
		t.Fatalf("ServerFrame request body request_id = %q, want %q", serverFrame.GetRequestBody().GetRequestId(), "req")
	}
	serverFrame = &ServerFrame{Msg: &ServerFrame_RequestEnd{RequestEnd: &RequestEnd{RequestId: "req"}}}
	if serverFrame.GetRequestEnd().GetRequestId() != "req" {
		t.Fatalf("ServerFrame request end request_id = %q, want %q", serverFrame.GetRequestEnd().GetRequestId(), "req")
	}
	serverFrame = &ServerFrame{Msg: &ServerFrame_CancelRequest{CancelRequest: &CancelRequest{RequestId: "req"}}}
	if serverFrame.GetCancelRequest().GetRequestId() != "req" {
		t.Fatalf("ServerFrame cancel request_id = %q, want %q", serverFrame.GetCancelRequest().GetRequestId(), "req")
	}
	serverFrame = &ServerFrame{Msg: &ServerFrame_Ping{Ping: &Ping{UnixNano: 13}}}
	if serverFrame.GetPing().GetUnixNano() != 13 {
		t.Fatalf("ServerFrame ping = %d, want 13", serverFrame.GetPing().GetUnixNano())
	}
	serverFrame = &ServerFrame{Msg: &ServerFrame_WsData{WsData: &WebSocketData{RequestId: "ws-1"}}}
	if serverFrame.GetWsData().GetRequestId() != "ws-1" {
		t.Fatalf("ServerFrame ws data request_id = %q, want %q", serverFrame.GetWsData().GetRequestId(), "ws-1")
	}
	serverFrame = &ServerFrame{Msg: &ServerFrame_WsClose{WsClose: &WebSocketClose{RequestId: "ws-1"}}}
	if serverFrame.GetWsClose().GetRequestId() != "ws-1" {
		t.Fatalf("ServerFrame ws close request_id = %q, want %q", serverFrame.GetWsClose().GetRequestId(), "ws-1")
	}

	var nilHeader *Header
	if nilHeader.GetKey() != "" || nilHeader.GetValues() != nil {
		t.Fatal("nil Header getters returned unexpected values")
	}
	if got := nilHeader.ProtoReflect().Descriptor().FullName(); got != "tunnel.v1.Header" {
		t.Fatalf("nil Header full name = %q, want %q", got, "tunnel.v1.Header")
	}
	var nilClientFrame *ClientFrame
	if nilClientFrame.GetMsg() != nil || nilClientFrame.GetRegister() != nil || nilClientFrame.GetPong() != nil || nilClientFrame.GetResponseStart() != nil || nilClientFrame.GetResponseBody() != nil || nilClientFrame.GetResponseEnd() != nil || nilClientFrame.GetResponseError() != nil || nilClientFrame.GetWsData() != nil || nilClientFrame.GetWsClose() != nil {
		t.Fatal("nil ClientFrame getters returned unexpected values")
	}
	if got := nilClientFrame.ProtoReflect().Descriptor().FullName(); got != "tunnel.v1.ClientFrame" {
		t.Fatalf("nil ClientFrame full name = %q, want %q", got, "tunnel.v1.ClientFrame")
	}
	var nilServerFrame *ServerFrame
	if nilServerFrame.GetMsg() != nil || nilServerFrame.GetRequestStart() != nil || nilServerFrame.GetRequestBody() != nil || nilServerFrame.GetRequestEnd() != nil || nilServerFrame.GetCancelRequest() != nil || nilServerFrame.GetPing() != nil || nilServerFrame.GetWsData() != nil || nilServerFrame.GetWsClose() != nil {
		t.Fatal("nil ServerFrame getters returned unexpected values")
	}
	if got := nilServerFrame.ProtoReflect().Descriptor().FullName(); got != "tunnel.v1.ServerFrame" {
		t.Fatalf("nil ServerFrame full name = %q, want %q", got, "tunnel.v1.ServerFrame")
	}

	var nilRegister *Register
	if nilRegister.GetTunnelId() != "" || nilRegister.GetToken() != "" {
		t.Fatal("nil Register getters returned unexpected values")
	}
	var nilPing *Ping
	if nilPing.GetUnixNano() != 0 {
		t.Fatal("nil Ping getter returned unexpected value")
	}
	var nilPong *Pong
	if nilPong.GetUnixNano() != 0 {
		t.Fatal("nil Pong getter returned unexpected value")
	}
	var nilRequestStart *RequestStart
	if nilRequestStart.GetRequestId() != "" || nilRequestStart.GetMethod() != "" || nilRequestStart.GetScheme() != "" || nilRequestStart.GetHost() != "" || nilRequestStart.GetPath() != "" || nilRequestStart.GetRawQuery() != "" || nilRequestStart.GetHeaders() != nil || nilRequestStart.GetRemoteAddr() != "" {
		t.Fatal("nil RequestStart getters returned unexpected values")
	}
	var nilRequestBody *RequestBody
	if nilRequestBody.GetRequestId() != "" || nilRequestBody.GetChunk() != nil {
		t.Fatal("nil RequestBody getters returned unexpected values")
	}
	var nilRequestEnd *RequestEnd
	if nilRequestEnd.GetRequestId() != "" {
		t.Fatal("nil RequestEnd getter returned unexpected value")
	}
	var nilCancelRequest *CancelRequest
	if nilCancelRequest.GetRequestId() != "" {
		t.Fatal("nil CancelRequest getter returned unexpected value")
	}
	var nilWebSocketData *WebSocketData
	if nilWebSocketData.GetRequestId() != "" || nilWebSocketData.GetPayload() != nil {
		t.Fatal("nil WebSocketData getters returned unexpected values")
	}
	var nilWebSocketClose *WebSocketClose
	if nilWebSocketClose.GetRequestId() != "" {
		t.Fatal("nil WebSocketClose getter returned unexpected value")
	}
	var nilResponseStart *ResponseStart
	if nilResponseStart.GetRequestId() != "" || nilResponseStart.GetStatusCode() != 0 || nilResponseStart.GetHeaders() != nil {
		t.Fatal("nil ResponseStart getters returned unexpected values")
	}
	var nilResponseBody *ResponseBody
	if nilResponseBody.GetRequestId() != "" || nilResponseBody.GetChunk() != nil {
		t.Fatal("nil ResponseBody getters returned unexpected values")
	}
	var nilResponseEnd *ResponseEnd
	if nilResponseEnd.GetRequestId() != "" {
		t.Fatal("nil ResponseEnd getter returned unexpected value")
	}
	var nilResponseError *ResponseError
	if nilResponseError.GetRequestId() != "" || nilResponseError.GetMessage() != "" {
		t.Fatal("nil ResponseError getters returned unexpected values")
	}

	callNoArg((&ClientFrame_Register{Register: &Register{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_Pong{Pong: &Pong{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_ResponseStart{ResponseStart: &ResponseStart{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_ResponseBody{ResponseBody: &ResponseBody{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_ResponseEnd{ResponseEnd: &ResponseEnd{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_ResponseError{ResponseError: &ResponseError{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_WsData{WsData: &WebSocketData{}}).isClientFrame_Msg)
	callNoArg((&ClientFrame_WsClose{WsClose: &WebSocketClose{}}).isClientFrame_Msg)
	callNoArg((&ServerFrame_RequestStart{RequestStart: &RequestStart{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_RequestBody{RequestBody: &RequestBody{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_RequestEnd{RequestEnd: &RequestEnd{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_CancelRequest{CancelRequest: &CancelRequest{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_Ping{Ping: &Ping{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_WsData{WsData: &WebSocketData{}}).isServerFrame_Msg)
	callNoArg((&ServerFrame_WsClose{WsClose: &WebSocketClose{}}).isServerFrame_Msg)

	if File_proto_tunnel_proto == nil {
		t.Fatal("File_proto_tunnel_proto = nil, want descriptor")
	}
}

func TestGRPCClientAndServer(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	server := grpc.NewServer()
	RegisterTunnelServiceServer(server, protoTestService{})
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("DialContext error: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	client := NewTunnelServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect error: %v", err)
	}
	if err := stream.Send(&ClientFrame{Msg: &ClientFrame_Register{Register: &Register{Token: "demo-token"}}}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	frame, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv error: %v", err)
	}
	if ping := frame.GetPing(); ping == nil || ping.GetUnixNano() != 99 {
		t.Fatalf("ping = %#v, want unix_nano 99", ping)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend error: %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv error = %v, want %v", err, io.EOF)
	}
}

func TestUnimplementedTunnelServiceServer(t *testing.T) {
	t.Parallel()

	err := (UnimplementedTunnelServiceServer{}).Connect(nil)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Connect error code = %v, want %v", status.Code(err), codes.Unimplemented)
	}
	callNoArg((UnimplementedTunnelServiceServer{}).mustEmbedUnimplementedTunnelServiceServer)
	callNoArg((protoTestService{}).mustEmbedUnimplementedTunnelServiceServer)
}
