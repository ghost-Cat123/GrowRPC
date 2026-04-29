package GrowRPC

import (
	"context"
	"fmt"
	"testing"
)

type Foo int

type Args struct{ Num1, Num2 int }

func (f *Foo) Sum(_ context.Context, args *Args, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func _assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assertion failed: "+msg, v...))
	}
}

func TestRegisterMethod(t *testing.T) {
	server := NewServer()
	var foo Foo
	RegisterMethod[Args, int](server, "Foo.Sum", foo.Sum)
	handlerI, ok := server.serviceMap.Load("Foo.Sum")
	_assert(ok, "service Method should be registered")
	_assert(handlerI != nil, "wrong Method, Sum shouldn't nil")
}

func TestMethodHandler_Call(t *testing.T) {
	server := NewServer()
	var foo Foo
	RegisterMethod[Args, int](server, "Foo.Sum", foo.Sum)
	handlerI, _ := server.serviceMap.Load("Foo.Sum")
	handler := handlerI.(MethodHandler)

	decodeFunc := func(v interface{}) error {
		args := v.(*Args)
		args.Num1 = 1
		args.Num2 = 3
		return nil
	}

	replyInter, err := handler(context.Background(), decodeFunc)
	reply := replyInter.(*int)
	_assert(err == nil && *reply == 4, "failed to call Foo.Sum")
}
