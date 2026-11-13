package grpcload

import (
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/optimm/Gmeter/internal/grpcdemo"
)

// oneFileAtATime is server reflection that sends only the file asked for and
// never what it imports, as some servers do: the client has to ask for each
// import itself.
type oneFileAtATime struct {
	rpb.UnimplementedServerReflectionServer
	files *protoregistry.Files
	asked []string
}

func (r *oneFileAtATime) ServerReflectionInfo(stream rpb.ServerReflection_ServerReflectionInfoServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		var fd protoreflect.FileDescriptor
		switch q := req.MessageRequest.(type) {
		case *rpb.ServerReflectionRequest_FileContainingSymbol:
			r.asked = append(r.asked, "symbol "+q.FileContainingSymbol)
			if d, err := r.files.FindDescriptorByName(protoreflect.FullName(q.FileContainingSymbol)); err == nil {
				fd = d.ParentFile()
			}
		case *rpb.ServerReflectionRequest_FileByFilename:
			r.asked = append(r.asked, "file "+q.FileByFilename)
			fd, _ = r.files.FindFileByPath(q.FileByFilename)
		}
		resp := &rpb.ServerReflectionResponse{}
		if fd == nil {
			resp.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{ErrorResponse: &rpb.ErrorResponse{ErrorCode: int32(codes.NotFound), ErrorMessage: "not found"}}
		} else {
			raw, _ := proto.Marshal(protodesc.ToFileDescriptorProto(fd))
			resp.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: [][]byte{raw}}}
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func TestReflectionFollowsTheImportsOfTheSchema(t *testing.T) {
	files, _ := grpcdemo.Files()
	refl := &oneFileAtATime{files: files}
	s := grpc.NewServer()
	echo := grpcdemo.Register(s, grpcdemo.NoReflection)
	rpb.RegisterServerReflectionServer(s, refl)
	addr := listen(t, s)

	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say, Data: []byte(`{"name":"cy"}`)})
	if code, err := call(t, c); code != "OK" || err != nil {
		t.Fatalf("call = %q, %v", code, err)
	}
	if strings.Join(refl.asked, "; ") != "symbol gmeter.demo.Echo; file gmeter/demo/reply.proto" {
		t.Errorf("asked %q; want the service, then the file it imports", refl.asked)
	}
	if echo.Names()["cy"] != 1 {
		t.Errorf("names = %v", echo.Names())
	}
}

func TestAProtosetWithoutItsImportsIsRefused(t *testing.T) {
	addr, _, _ := serve(t, grpcdemo.NoReflection)
	files, _ := grpcdemo.Files()
	echoFile, err := files.FindFileByPath("gmeter/demo/echo.proto")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(echoFile)}})
	_, err = dial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say, Protoset: raw})
	if err == nil || !strings.Contains(err.Error(), "--include_imports") {
		t.Errorf("err = %v; want a hint to build it with --include_imports", err)
	}
}

func TestTheOptionsAreChecked(t *testing.T) {
	addr, echo, _ := serve(t, grpcdemo.ReflectionBoth)
	_, err := dial(t, Options{Addr: addr, Plaintext: true, TLS: insecureTLS(), Method: grpcdemo.Say})
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("plaintext with a TLS configuration: %v", err)
	}
	_, err = dial(t, Options{Addr: addr, Plaintext: true, Method: "gmeter.demo.Nope/Say"})
	if err == nil || strings.Contains(err.Error(), "grpc.reflection") {
		t.Errorf("the services listed include reflection itself: %v", err)
	}
	c := mustDial(t, Options{Addr: addr, Plaintext: true, Method: grpcdemo.Say})
	for range 5 {
		call(t, c)
	}
	if echo.Peers() != 1 {
		t.Errorf("%d connections by default, want 1", echo.Peers())
	}
}
