// Package grpcdemo is a small gRPC service to load-test: the tests of gmeter's
// gRPC support call it, and so can a demo. Its schema is built here, from a
// descriptor written out by hand, so that neither protoc nor generated code is
// needed.
//
//	// gmeter/demo/reply.proto
//	package gmeter.demo;
//	message SayReply { string message = 1; }
//
//	// gmeter/demo/echo.proto
//	package gmeter.demo;
//	import "gmeter/demo/reply.proto";
//	message SayRequest { string name = 1; int32 delay_ms = 2; string code = 3; }
//	service Echo {
//	  rpc Say(SayRequest) returns (SayReply);                    // waits delay_ms, then answers "hello <name>", or fails with code
//	  rpc Chat(stream SayRequest) returns (stream SayReply);     // streaming, which gmeter does not load-test
//	}
package grpcdemo

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Service is the full name of the Echo service, and Say its unary method.
const (
	Service = "gmeter.demo.Echo"
	Say     = Service + "/Say"
)

// schema is the descriptors of the schema above: echo.proto imports
// reply.proto, so that reading the schema has an import to follow.
func schema() []*descriptorpb.FileDescriptorProto {
	field := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Type: typ.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String(jsonName(name)),
		}
	}
	str, i32 := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_INT32
	reply := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("gmeter/demo/reply.proto"),
		Package:     proto.String("gmeter.demo"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("SayReply"), Field: []*descriptorpb.FieldDescriptorProto{field("message", 1, str)}}},
	}
	echo := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("gmeter/demo/echo.proto"),
		Package:    proto.String("gmeter.demo"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"gmeter/demo/reply.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("SayRequest"), Field: []*descriptorpb.FieldDescriptorProto{field("name", 1, str), field("delay_ms", 2, i32), field("code", 3, str)}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Echo"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("Say"), InputType: proto.String(".gmeter.demo.SayRequest"), OutputType: proto.String(".gmeter.demo.SayReply")},
				{Name: proto.String("Chat"), InputType: proto.String(".gmeter.demo.SayRequest"), OutputType: proto.String(".gmeter.demo.SayReply"),
					ClientStreaming: proto.Bool(true), ServerStreaming: proto.Bool(true)},
			},
		}},
	}
	return []*descriptorpb.FileDescriptorProto{echo, reply}
}

func jsonName(s string) string {
	out := []byte{}
	up := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '_':
			up = true
		case up && c >= 'a' && c <= 'z':
			out, up = append(out, c-'a'+'A'), false
		default:
			out, up = append(out, c), false
		}
	}
	return string(out)
}

// Files is the schema, as a registry and as a serialized FileDescriptorSet
// (what `protoc --descriptor_set_out --include_imports` writes).
func Files() (*protoregistry.Files, []byte) {
	set := &descriptorpb.FileDescriptorSet{File: schema()}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		panic(fmt.Sprintf("grpcdemo: the schema does not build: %v", err))
	}
	raw, err := proto.Marshal(set)
	if err != nil {
		panic(err)
	}
	return files, raw
}

// Reflection says which server reflection versions to offer.
type Reflection int

const (
	NoReflection Reflection = iota
	ReflectionV1Alpha
	ReflectionBoth // v1 and v1alpha, as grpc-go's reflection.Register does
)

// Echo is the service. It counts what it was sent.
type Echo struct {
	Calls atomic.Int64

	mu    sync.Mutex
	names map[string]int // SayRequest.name -> calls
	peers map[string]bool
	md    metadata.MD // of the last call
}

// Register adds Echo to s, with the server reflection asked for.
func Register(s *grpc.Server, r Reflection) *Echo {
	e := &Echo{names: map[string]int{}, peers: map[string]bool{}}
	files, _ := Files()
	d, err := files.FindDescriptorByName(Service)
	if err != nil {
		panic(err)
	}
	methods := d.(protoreflect.ServiceDescriptor).Methods()
	say, chat := methods.ByName("Say"), methods.ByName("Chat")
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: Service,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Say", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			in := dynamicpb.NewMessage(say.Input())
			if err := dec(in); err != nil {
				return nil, err
			}
			return e.say(ctx, in, say.Output())
		}}},
		Streams: []grpc.StreamDesc{{StreamName: "Chat", ClientStreams: true, ServerStreams: true, Handler: func(_ any, st grpc.ServerStream) error {
			for {
				in := dynamicpb.NewMessage(chat.Input())
				if err := st.RecvMsg(in); err == io.EOF {
					return nil
				} else if err != nil {
					return err
				}
				out, err := e.say(st.Context(), in, chat.Output())
				if err != nil {
					return err
				}
				if err := st.SendMsg(out); err != nil {
					return err
				}
			}
		}}},
		Metadata: "gmeter/demo/echo.proto",
	}, e)
	opts := reflection.ServerOptions{Services: s, DescriptorResolver: files, ExtensionResolver: protoregistry.GlobalTypes}
	switch r {
	case ReflectionBoth:
		reflectionv1.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
		reflectionv1alpha.RegisterServerReflectionServer(s, reflection.NewServer(opts))
	case ReflectionV1Alpha:
		reflectionv1alpha.RegisterServerReflectionServer(s, reflection.NewServer(opts))
	}
	return e
}

func (e *Echo) say(ctx context.Context, in *dynamicpb.Message, out protoreflect.MessageDescriptor) (proto.Message, error) {
	e.Calls.Add(1)
	fields := in.Descriptor().Fields()
	name := in.Get(fields.ByName("name")).String()
	e.mu.Lock()
	e.names[name]++
	if p, ok := peer.FromContext(ctx); ok {
		e.peers[p.Addr.String()] = true
	}
	e.md, _ = metadata.FromIncomingContext(ctx)
	e.mu.Unlock()

	if d := in.Get(fields.ByName("delay_ms")).Int(); d > 0 {
		select {
		case <-time.After(time.Duration(d) * time.Millisecond):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if c := in.Get(fields.ByName("code")).String(); c != "" {
		return nil, status.Error(codeNamed(c), "asked for "+c)
	}
	reply := dynamicpb.NewMessage(out)
	reply.Set(out.Fields().ByName("message"), protoreflect.ValueOfString("hello "+name))
	return reply, nil
}

// codeNamed is the code of a name such as "NOT_FOUND"; an unknown name is UNKNOWN.
func codeNamed(name string) codes.Code {
	var c codes.Code
	if err := c.UnmarshalJSON([]byte(`"` + name + `"`)); err != nil {
		return codes.Unknown
	}
	return c
}

// Names is how many calls asked for each name.
func (e *Echo) Names() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.names))
	for k, v := range e.names {
		out[k] = v
	}
	return out
}

// Peers is how many client connections the calls came over.
func (e *Echo) Peers() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.peers)
}

// Metadata is the metadata of the last call.
func (e *Echo) Metadata() metadata.MD {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.md.Copy()
}
