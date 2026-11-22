// Package grpcload makes the calls of a gRPC load test: one unary or streaming
// method, found by server reflection or in a descriptor set, called again and
// again with one request built from JSON. A Caller's Call is what
// runner.Config.Call runs for every request of the schedule: one scheduled
// call is one whole stream for a streaming method.
package grpcload

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/optimm/Gmeter/internal/runner"
)

// Options says what to call and how to reach it.
type Options struct {
	Addr   string // host:port
	Method string // "package.Service/Method" (or "package.Service.Method")
	Data   []byte // the request, as protobuf JSON; empty is the empty message

	// Protoset is a serialized FileDescriptorSet with the method's service and
	// everything it imports (protoc --descriptor_set_out --include_imports).
	// Nil asks the server, by server reflection.
	Protoset []byte

	// Plaintext speaks HTTP/2 without TLS. Otherwise TLS is used, with TLS as
	// its configuration, or the system's roots if TLS is nil.
	Plaintext bool
	TLS       *tls.Config

	Conns    int         // connections to spread the calls over; 0 is 1
	Metadata metadata.MD // sent with every call

	// SendMessages is how many request messages each stream sends. Zero is one
	// for a client or bidirectional stream. A server-streaming method always
	// sends one, and a unary method none.
	SendMessages int
	// RecvMessages is how many response messages each stream reads before it is
	// ended. Zero reads until the server ends the stream.
	RecvMessages int
}

// Caller calls one method over a few open connections.
type Caller struct {
	conns   []*grpc.ClientConn
	next    atomic.Uint64
	path    string // "/package.Service/Method"
	payload []byte // the request, encoded once
	md      metadata.MD

	clientStreams bool
	serverStreams bool
	stream        *grpc.StreamDesc // nil for a unary method
	send, recv    int
}

// Dial opens the connections, waits until every one is ready (so connecting is
// not part of the test), finds the method and builds the request. ctx bounds
// all of it. Nothing is sent to the method itself.
func Dial(ctx context.Context, o Options) (*Caller, error) {
	service, method, err := splitMethod(o.Method)
	if err != nil {
		return nil, err
	}
	switch {
	case o.SendMessages < 0:
		return nil, fmt.Errorf("grpc: -stream-send must be at least 0, got %d", o.SendMessages)
	case o.RecvMessages < 0:
		return nil, fmt.Errorf("grpc: -stream-recv must be at least 0, got %d", o.RecvMessages)
	}
	switch {
	case o.Conns < 0:
		return nil, fmt.Errorf("grpc: connections must be at least 1, got %d", o.Conns)
	case o.Conns == 0:
		o.Conns = 1
	}
	if o.Plaintext && o.TLS != nil {
		return nil, errors.New("grpc: a plaintext connection has no TLS to configure")
	}
	var files *protoregistry.Files
	if o.Protoset != nil {
		if files, err = parseProtoset(o.Protoset); err != nil {
			return nil, err
		}
	}

	c := &Caller{md: o.Metadata}
	var last lastError
	for range o.Conns {
		conn, err := newConn(o, &last)
		if err != nil {
			c.Close()
			return nil, err
		}
		c.conns = append(c.conns, conn)
	}
	if err := c.ready(ctx, o.Addr, &last); err != nil {
		c.Close()
		return nil, err
	}
	if files == nil {
		if files, err = reflectFiles(ctx, c.conns[0], o.Addr, service); err != nil {
			c.Close()
			return nil, err
		}
	}
	md, err := findMethod(files, service, method)
	if err == nil {
		c.payload, err = request(md.Input(), o.Data)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	c.path = "/" + string(md.Parent().FullName()) + "/" + string(md.Name())

	c.clientStreams = md.IsStreamingClient()
	c.serverStreams = md.IsStreamingServer()
	if c.clientStreams || c.serverStreams {
		c.stream = &grpc.StreamDesc{StreamName: string(md.Name()), ClientStreams: c.clientStreams, ServerStreams: c.serverStreams}
	}
	send, recv, err := normalizeCounts(c.path, c.clientStreams, c.serverStreams, o.SendMessages, o.RecvMessages)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.send, c.recv = send, recv
	return c, nil
}

// splitMethod reads "package.Service/Method" or "package.Service.Method".
func splitMethod(s string) (service, method string, err error) {
	s = strings.TrimPrefix(s, "/")
	i := strings.LastIndex(s, "/")
	if i < 0 {
		i = strings.LastIndex(s, ".")
	}
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("grpc: want the method as package.Service/Method, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

func parseProtoset(raw []byte) (*protoregistry.Files, error) {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("grpc: the protoset is not a FileDescriptorSet: %w", err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("grpc: the protoset does not hold together (was it made with --include_imports?): %w", err)
	}
	return files, nil
}

// lastError remembers why connecting last failed: gRPC itself only says that a
// connection is in TRANSIENT_FAILURE, not why.
type lastError struct {
	mu  sync.Mutex
	err error
}

func (l *lastError) set(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (l *lastError) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// recordingCreds notes a failed TLS handshake.
type recordingCreds struct {
	credentials.TransportCredentials
	last *lastError
}

func (r recordingCreds) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := r.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		r.last.set(fmt.Errorf("TLS handshake: %w", err))
	}
	return conn, info, err
}

func (r recordingCreds) Clone() credentials.TransportCredentials {
	return recordingCreds{r.TransportCredentials.Clone(), r.last}
}

func newConn(o Options, last *lastError) (*grpc.ClientConn, error) {
	creds := insecure.NewCredentials()
	if !o.Plaintext {
		cfg := o.TLS
		if cfg == nil {
			cfg = &tls.Config{}
		}
		creds = credentials.NewTLS(cfg)
	}
	var dialer net.Dialer
	return grpc.NewClient("passthrough:///"+o.Addr,
		grpc.WithTransportCredentials(recordingCreds{creds, last}),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				last.set(err)
			}
			return conn, err
		}),
	)
}

// ready waits until every connection is ready, or one fails.
func (c *Caller) ready(ctx context.Context, addr string, last *lastError) error {
	for _, conn := range c.conns {
		conn.Connect()
		for {
			s := conn.GetState()
			if s == connectivity.Ready {
				break
			}
			if s == connectivity.TransientFailure || s == connectivity.Shutdown {
				return connectError(addr, last)
			}
			if !conn.WaitForStateChange(ctx, s) {
				return fmt.Errorf("grpc: could not connect to %s: %w", addr, ctx.Err())
			}
		}
	}
	return nil
}

func connectError(addr string, last *lastError) error {
	if err := last.get(); err != nil {
		return fmt.Errorf("grpc: cannot connect to %s: %w", addr, err)
	}
	return fmt.Errorf("grpc: cannot connect to %s: the connection closed before the server spoke HTTP/2 (TLS on one side and not on the other?)", addr)
}

func findMethod(files *protoregistry.Files, service, method string) (protoreflect.MethodDescriptor, error) {
	d, _ := files.FindDescriptorByName(protoreflect.FullName(service))
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("grpc: there is no service %s; there are: %s", service, strings.Join(services(files), ", "))
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		var names []string
		for i := range sd.Methods().Len() {
			names = append(names, string(sd.Methods().Get(i).Name()))
		}
		return nil, fmt.Errorf("grpc: %s has no method %s; it has: %s", service, method, strings.Join(names, ", "))
	}
	return md, nil
}

// normalizeCounts validates and normalizes the send/recv counts for a method of
// the given streaming mode. It returns the counts to use.
func normalizeCounts(path string, clientStreams, serverStreams bool, send, recv int) (int, int, error) {
	unary := !clientStreams && !serverStreams
	serverStreaming := !clientStreams && serverStreams
	switch {
	case unary:
		if send != 0 || recv != 0 {
			return 0, 0, fmt.Errorf("grpc: %s is a unary method, so -stream-send and -stream-recv have nothing to do", path)
		}
	case serverStreaming:
		if send < 0 {
			return 0, 0, fmt.Errorf("grpc: -stream-send must be at least 0, got %d", send)
		}
		if send > 1 {
			return 0, 0, fmt.Errorf("grpc: %s is a server-streaming method, which sends one request per stream; -stream-send %d is too many", path, send)
		}
		if send == 0 {
			send = 1
		}
	default: // client or bidi
		if send < 0 {
			return 0, 0, fmt.Errorf("grpc: -stream-send must be at least 0, got %d", send)
		}
		if send == 0 {
			send = 1
		}
	}
	if recv < 0 {
		return 0, 0, fmt.Errorf("grpc: -stream-recv must be at least 0, got %d", recv)
	}
	return send, recv, nil
}

func services(files *protoregistry.Files) []string {
	var out []string
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			out = append(out, string(f.Services().Get(i).FullName()))
		}
		return true
	})
	slices.Sort(out)
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// request encodes data, protobuf JSON, as a message of type in. Unknown fields
// are an error: a misspelt field would otherwise send the empty value.
func request(in protoreflect.MessageDescriptor, data []byte) ([]byte, error) {
	msg := dynamicpb.NewMessage(in)
	if len(data) > 0 {
		if err := protojson.Unmarshal(data, msg); err != nil {
			return nil, fmt.Errorf("grpc: the request is not a %s: %w", in.FullName(), err)
		}
	}
	return proto.Marshal(msg)
}

// Method is the method called, as gRPC names it: "/package.Service/Method".
func (c *Caller) Method() string { return c.path }

// Streaming is "", "server", "client" or "bidi".
func (c *Caller) Streaming() string {
	switch {
	case c.clientStreams && c.serverStreams:
		return "bidi"
	case c.clientStreams:
		return "client"
	case c.serverStreams:
		return "server"
	default:
		return ""
	}
}

// StreamMessages is how many request messages each stream sends and how many
// responses it reads before ending (0: until the server closes). Both are 0
// for a unary method.
func (c *Caller) StreamMessages() (send, recv int) {
	if c.stream == nil {
		return 0, 0
	}
	return c.send, c.recv
}

// Call makes one call (or one whole stream, for a streaming method). It returns
// the name of the status code the server answered with ("OK", "NOT_FOUND", ...),
// or an error when there was no answer: ctx's own error when ctx ended first,
// and a runner.Failure of kind "unavailable" for UNAVAILABLE, which is also
// what gRPC says when a connection failed, so it cannot be told apart from a
// server's answer.
func (c *Caller) Call(ctx context.Context) (string, error) {
	if c.stream != nil {
		return c.callStream(ctx)
	}
	return c.callUnary(ctx)
}

func (c *Caller) callUnary(ctx context.Context) (string, error) {
	if len(c.md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, c.md)
	}
	conn := c.conns[(c.next.Add(1)-1)%uint64(len(c.conns))]
	err := conn.Invoke(ctx, c.path, c.payload, discard{}, grpc.ForceCodecV2(rawCodec{}))
	if err == nil {
		return "OK", nil
	}
	return c.outcome(ctx, err)
}

// outcome turns the result of a call or stream into the status name, or an
// error when there was no answer. io.EOF is handled by the streaming caller.
func (c *Caller) outcome(ctx context.Context, err error) (string, error) {
	st, ok := status.FromError(err)
	if !ok {
		return "", err
	}
	switch st.Code() {
	case codes.DeadlineExceeded, codes.Canceled:
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// The server's DEADLINE_EXCEEDED for this call's own timeout can arrive
		// a moment before the context's timer marks it done; it is still ours.
		if dl, ok := ctx.Deadline(); ok && st.Code() == codes.DeadlineExceeded && !time.Now().Before(dl) {
			return "", context.DeadlineExceeded
		}
	case codes.Unavailable:
		return "", runner.Failure{Kind: "unavailable", Err: err}
	}
	return CodeName(st.Code()), nil
}

func (c *Caller) callStream(ctx context.Context) (string, error) {
	if len(c.md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, c.md)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := c.conns[(c.next.Add(1)-1)%uint64(len(c.conns))]
	st, err := conn.NewStream(ctx, c.stream, c.path, grpc.ForceCodecV2(rawCodec{}))
	if err != nil {
		return c.outcome(ctx, err)
	}

	var sendErr error
	for range c.send {
		if err := st.SendMsg(c.payload); err != nil {
			sendErr = err
			break
		}
	}
	if sendErr == nil {
		if err := st.CloseSend(); err != nil {
			sendErr = err
		}
	}
	// A non-EOF send/close error is final. io.EOF means the server ended the
	// stream; RecvMsg below returns the status that says why.
	if sendErr != nil && !errors.Is(sendErr, io.EOF) {
		return c.outcome(ctx, sendErr)
	}

	got := 0
	for c.recv == 0 || got < c.recv {
		err := st.RecvMsg(discard{})
		if errors.Is(err, io.EOF) {
			return "OK", nil
		}
		if err != nil {
			return c.outcome(ctx, err)
		}
		got++
	}
	return "OK", nil
}

// Close closes the connections.
func (c *Caller) Close() error {
	var errs []error
	for _, conn := range c.conns {
		errs = append(errs, conn.Close())
	}
	return errors.Join(errs...)
}

// CodeName is a code's name as gRPC's specification spells it, e.g. NOT_FOUND.
func CodeName(c codes.Code) string {
	if int(c) < len(codeNames) {
		return codeNames[c]
	}
	return fmt.Sprintf("CODE_%d", c)
}

var codeNames = []string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS",
	"PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE",
	"UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
}

// rawCodec sends the request as it was encoded once, and does not decode the
// answer: a load test needs to know that the call succeeded, not what it said.
type rawCodec struct{}

type discard struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("grpc: cannot send a %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(b)}, nil
}

func (rawCodec) Unmarshal(mem.BufferSlice, any) error { return nil }

func (rawCodec) Name() string { return "proto" }
