package grpcload

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// The two versions of server reflection have the same messages on the wire,
// so the v1 messages serve for both; only the method differs.
var reflectionMethods = []string{
	"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
}

// reflectFiles asks the server at addr, by server reflection, for the file
// that defines service and every file it imports.
func reflectFiles(ctx context.Context, conn *grpc.ClientConn, addr, service string) (*protoregistry.Files, error) {
	for _, method := range reflectionMethods {
		files, err := reflectAt(ctx, conn, method, service)
		if status.Code(err) == codes.Unimplemented {
			continue // try the older version
		}
		return files, err
	}
	return nil, fmt.Errorf("grpc: %s does not offer server reflection, so it cannot say what %s is: give its schema with -protoset FILE (protoc --descriptor_set_out=FILE --include_imports ...)", addr, service)
}

func reflectAt(ctx context.Context, conn *grpc.ClientConn, method, service string) (*protoregistry.Files, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the stream
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method)
	if err != nil {
		return nil, err
	}
	ask := func(req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
		if err := stream.SendMsg(req); err != nil {
			return nil, err
		}
		resp := new(rpb.ServerReflectionResponse)
		if err := stream.RecvMsg(resp); err != nil {
			return nil, err
		}
		return resp, nil
	}

	resp, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: service}})
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		if codes.Code(e.ErrorCode) != codes.NotFound {
			return nil, fmt.Errorf("grpc: server reflection could not describe %s: %s", service, e.ErrorMessage)
		}
		known, err := listServices(ask)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("grpc: there is no service %s; there are: %s", service, strings.Join(known, ", "))
	}

	got := map[string]*descriptorpb.FileDescriptorProto{}
	add := func(r *rpb.ServerReflectionResponse) error {
		for _, raw := range r.GetFileDescriptorResponse().GetFileDescriptorProto() {
			fd := new(descriptorpb.FileDescriptorProto)
			if err := proto.Unmarshal(raw, fd); err != nil {
				return fmt.Errorf("grpc: server reflection sent a broken file descriptor: %w", err)
			}
			got[fd.GetName()] = fd
		}
		return nil
	}
	if err := add(resp); err != nil {
		return nil, err
	}
	for {
		var missing []string
		for _, fd := range got {
			for _, dep := range fd.GetDependency() {
				if got[dep] == nil && !slices.Contains(missing, dep) {
					missing = append(missing, dep)
				}
			}
		}
		if len(missing) == 0 {
			break
		}
		for _, name := range missing {
			resp, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: name}})
			if err != nil {
				return nil, err
			}
			if e := resp.GetErrorResponse(); e != nil {
				return nil, fmt.Errorf("grpc: server reflection does not have %s, which the schema of %s imports: %s", name, service, e.ErrorMessage)
			}
			if err := add(resp); err != nil {
				return nil, err
			}
		}
	}
	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range got {
		set.File = append(set.File, fd)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("grpc: the schema server reflection sent does not hold together: %w", err)
	}
	return files, nil
}

// listServices is the services the server offers, leaving out reflection itself.
func listServices(ask func(*rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error)) ([]string, error) {
	resp, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: "*"}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		if !strings.HasPrefix(s.GetName(), "grpc.reflection.") {
			out = append(out, s.GetName())
		}
	}
	slices.Sort(out)
	if len(out) == 0 {
		out = []string{"none"}
	}
	return out, nil
}
