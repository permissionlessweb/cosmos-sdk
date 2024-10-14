package implementation

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"cosmossdk.io/core/transaction"
)

// RegisterInitHandler registers an initialisation handler for a smart account that uses protobuf.
func RegisterInitHandler[
	Req any, ProtoReq ProtoMsgG[Req], Resp any, ProtoResp ProtoMsgG[Resp],
](router *InitBuilder, handler func(ctx context.Context, req ProtoReq) (ProtoResp, error),
) {
	reqName := MessageName(ProtoReq(new(Req)))

	router.handler = func(ctx context.Context, initRequest transaction.Msg) (initResponse transaction.Msg, err error) {
		concrete, ok := initRequest.(ProtoReq)
		if !ok {
			return nil, fmt.Errorf("%w: wanted %s, got %T", errInvalidMessage, reqName, initRequest)
		}
		return handler(ctx, concrete)
	}

	router.schema = HandlerSchema{
		RequestSchema:  *NewProtoMessageSchema[Req, ProtoReq](),
		ResponseSchema: *NewProtoMessageSchema[Resp, ProtoResp](),
	}
}

// RegisterExecuteHandler registers an execution handler for a smart account that uses protobuf.
func RegisterExecuteHandler[
	Req any, ProtoReq ProtoMsgG[Req], Resp any, ProtoResp ProtoMsgG[Resp],
](router ProtoMsgHandlerRegistry, handler func(ctx context.Context, req ProtoReq) (ProtoResp, error),
) {
	RegisterHandler(router, handler)
}

// RegisterQueryHandler registers a query handler for a smart account that uses protobuf.
func RegisterQueryHandler[
	Req any, ProtoReq ProtoMsgG[Req], Resp any, ProtoResp ProtoMsgG[Resp],
](router *QueryBuilder, handler func(ctx context.Context, req ProtoReq) (ProtoResp, error),
) {
	RegisterHandler(router.er, handler)
}

func RegisterHandler[
	Req any, ProtoReq ProtoMsgG[Req], Resp any, ProtoResp ProtoMsgG[Resp],
](router ProtoMsgHandlerRegistry, handler func(ctx context.Context, req ProtoReq) (ProtoResp, error),
) {
	reqName := MessageName(ProtoReq(new(Req)))
	fn := func(ctx context.Context, executeRequest transaction.Msg) (executeResponse transaction.Msg, err error) {
		concrete, ok := executeRequest.(ProtoReq)
		if !ok {
			return nil, fmt.Errorf("%w: wanted %s, got %T", errInvalidMessage, reqName, executeRequest)
		}
		return handler(ctx, concrete)
	}
	schema := HandlerSchema{
		RequestSchema:  *NewProtoMessageSchema[Req, ProtoReq](),
		ResponseSchema: *NewProtoMessageSchema[Resp, ProtoResp](),
	}
	router.RegisterHandler(reqName, fn, schema)
}

func NewProtoMessageSchema[T any, PT ProtoMsgG[T]]() *MessageSchema {
	msg := PT(new(T))
	if _, ok := (interface{}(msg)).(proto.Message); ok {
		panic("protov2 messages are not supported")
	}
	return &MessageSchema{
		Name: MessageName(msg),
		New: func() transaction.Msg {
			return PT(new(T))
		},
	}
}
