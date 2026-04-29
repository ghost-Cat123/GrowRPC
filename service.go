package GrowRPC

import (
	"context"
)

// MethodHandler 定义底层的通用处理函数，请求参数反序列化交给内部的 decode 闭包
type MethodHandler func(ctx context.Context, decode func(interface{}) error) (interface{}, error)

// RegisterMethod 泛型注册接口
// 依靠泛型实例化 Req 和 Resp，彻底消除 reflect.New 和 reflect.Call
func RegisterMethod[Req any, Resp any](
	server *Server,
	serviceMethod string,
	handler func(ctx context.Context, req *Req, resp *Resp) error,
) {
	// 封装为无类型的 MethodHandler 存入 map
	wrapper := func(ctx context.Context, decode func(interface{}) error) (interface{}, error) {
		req := new(Req) // 泛型实例化，不需要 reflect
		if err := decode(req); err != nil {
			return nil, err
		}
		resp := new(Resp)
		err := handler(ctx, req, resp)
		return resp, err
	}
	server.serviceMap.Store(serviceMethod, MethodHandler(wrapper))
}
