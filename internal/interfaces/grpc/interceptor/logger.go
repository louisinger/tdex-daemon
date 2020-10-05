package interceptor

import (
	"context"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// UnaryLoggerInterceptor returns the unary interceptor with a logrus log
func UnaryLoggerInterceptor() grpc.ServerOption {
	return grpc.UnaryInterceptor(unaryLogger)
}

// StreamLoggerInterceptor returns the stream interceptor with a logrus log
func StreamLoggerInterceptor() grpc.ServerOption {
	return grpc.StreamInterceptor(streamLogger)
}

func unaryLogger(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	log.Debug(info.FullMethod)
	return handler(ctx, req)
}

func streamLogger(
	srv interface{},
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	log.Debug(info.FullMethod)
	return handler(srv, stream)
}