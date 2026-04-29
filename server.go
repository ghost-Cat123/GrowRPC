package GrowRPC

import (
	"GrowRPC/codec"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const MagicNumber = 0x3bef5c

// Option Option固定在报文开始 后面可接多个header和body
// option 使用JSON解码 之后使用CodecType解码剩余的头和消息体
type Option struct {
	// 标记这是一个RPC请求
	MagicNumber int
	// 用户可选的编码格式
	CodecType codec.Type
	// 连接超时时间
	ConnectTimeout time.Duration
	// 处理超时时间
	HandleTimeout time.Duration
}

var DefaultOption = &Option{
	MagicNumber: MagicNumber,
	// 默认编码方式Gob
	CodecType: codec.GobType,
	// 默认10s
	ConnectTimeout: time.Second * 10,
}

// Server RPC服务器
type Server struct {
	// 注册服务列表
	serviceMap sync.Map
	// 请求中间件列表
	interceptors []Interceptor
}

// NewServer Server构造函数
func NewServer() *Server {
	return &Server{}
}

// DefaultServer Server实例
var DefaultServer = NewServer()

// Use 服务使用中间件
func (server *Server) Use(interceptors ...Interceptor) {
	server.interceptors = append(server.interceptors, interceptors...)
}

// Use 默认实例调用
func Use(interceptors ...Interceptor) {
	DefaultServer.interceptors = append(DefaultServer.interceptors, interceptors...)
}

// Accept 监听服务器请求时接收链接
func (server *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Println("rpc server: accept error:", err)
			return
		}
		// 使用多线程链接多个服务
		go server.ServeConn(conn)
	}
}

func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

// ServeConn 服务连接
func (server *Server) ServeConn(conn io.ReadWriteCloser) {
	defer func() { _ = conn.Close() }()
	// 使用json反序列化得到option实例
	var opt Option
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("rpc server: options error: ", err)
		return
	}
	if opt.MagicNumber != MagicNumber {
		log.Printf("rpc server: invalid magic number %x", opt.MagicNumber)
		return
	}
	// 寻找相应编码格式的解析函数
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		log.Printf("rpc server: invalid codec type %s", opt.CodecType)
		return
	}
	server.serveCodec(f(conn))
}

// 错误发生时响应参数的占位符
var invalidRequest = struct{}{}

// 服务处理
func (server *Server) serveCodec(cc codec.Codec) {
	// 互斥锁
	sending := new(sync.Mutex)
	// 加锁 直到所有请求被处理 等待组
	wg := new(sync.WaitGroup)
	for {
		// 读取请求 一次连接 允许接收多个请求
		req, err := server.readRequest(cc)
		if err != nil {
			if req == nil {
				// 不可能恢复 则关闭连接
				break
			}
			req.h.Error = err.Error()
			// 回复请求 逐个发送 使用锁保证
			server.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}
		// 计数
		wg.Add(1)
		// 处理请求
		go server.handleRequest(cc, req, sending, wg, DefaultOption.ConnectTimeout)
	}
	wg.Wait()
	_ = cc.Close()
}

type Request struct {
	// 请求头
	h *codec.Header
	// 请求参数和响应值交给 wrapper 处理
	handler MethodHandler
}

// CallInfo 中间件相关
// CallInfo 暴露给中间件的只读参数
type CallInfo struct {
	Ctx           context.Context
	ServiceMethod string
	Header        *codec.Header
	ReqArgs       interface{}
}

// HandlerFunc 基本类型 只传请求
type HandlerFunc func(i *CallInfo) error

// Interceptor 中间件类型
type Interceptor func(next HandlerFunc) HandlerFunc

func (server *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
			log.Println("rpc server: read header error:", err)
		}
		return nil, err
	}
	return &h, nil
}

func (server *Server) readRequest(cc codec.Codec) (*Request, error) {
	h, err := server.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}
	req := &Request{h: h}
	req.handler, err = server.findHandler(h.ServiceMethod)
	if err != nil {
		return req, err
	}
	// 不在此处进行 cc.ReadBody，反序列化将推迟到 MethodHandler 内部执行
	return req, nil
}

func (server *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	// 加锁
	sending.Lock()
	// 解锁
	defer sending.Unlock()
	if err := cc.Write(h, body); err != nil {
		log.Println("rpc server: write response error:", err)
	}
}

// 正常情况 由处理方法调用的goroutine发送响应
// 超时情况 由主goroutine发送超时响应
// 使用拦截器处理请求
func (server *Server) handleRequest(cc codec.Codec, req *Request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	// 需要注册rpc方法到正确的响应中
	// 计数器-1 表示已处理完成一个响应
	defer wg.Done()
	// 拆分处理过程
	called := make(chan struct{}, 1)
	sent := make(chan struct{}, 1)
	go func() {
		// 方法调用
		// 添加拦截器拦截
		ctx := context.Background()
		var cancel context.CancelFunc
		if req.h.Metadata != nil {
			if deadlineStr, ok := req.h.Metadata["deadline"]; ok {
				if deadlineMs, err := strconv.ParseInt(deadlineStr, 10, 64); err == nil {
					ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(deadlineMs))
				}
			}
		}
		if cancel == nil {
			ctx, cancel = context.WithCancel(ctx)
		}
		defer cancel()

		info := &CallInfo{
			Ctx:           ctx,
			ServiceMethod: req.h.ServiceMethod,
			Header:        req.h,
			ReqArgs:       nil, // 泛型改造后由于不知道具体类型，置为空
		}

		var respData interface{}
		var handler HandlerFunc = func(i *CallInfo) error {
			// 闭包注入给泛型包装器，使其能读取反序列化
			decodeFunc := func(v interface{}) error {
				return cc.ReadBody(v)
			}
			resp, err := req.handler(i.Ctx, decodeFunc)
			respData = resp
			return err
		}

		// 组装中间件
		for i := len(server.interceptors) - 1; i >= 0; i-- {
			handler = server.interceptors[i](handler)
		}

		// 调用最后的方法
		err := handler(info)

		// 标记调用完成
		called <- struct{}{}
		// 错误分支
		if err != nil {
			req.h.Error = err.Error()
			// 发生错误 响应错误
			server.sendResponse(cc, req.h, invalidRequest, sending)
			sent <- struct{}{}
			return
		}
		// 成功分支
		server.sendResponse(cc, req.h, respData, sending)
		// 通过管道发送
		// 标记响应完成
		sent <- struct{}{}
	}()

	// 没有设置超时时间 就一直阻塞
	if timeout == 0 {
		<-called
		<-sent
		return
	}
	select {
	// 超时时间大于0 调用优先与超时完成
	case <-time.After(timeout):
		req.h.Error = fmt.Sprintf("rpc server: request handle timeout: expect within %s", timeout)
		server.sendResponse(cc, req.h, invalidRequest, sending)
	// 正常执行或发生异常
	case <-called:
		<-sent
	}
}

// 发现服务
func (server *Server) findHandler(serviceMethod string) (MethodHandler, error) {
	handlerI, ok := server.serviceMap.Load(serviceMethod)
	if !ok {
		return nil, errors.New("rpc server: can't find service method " + serviceMethod)
	}
	return handlerI.(MethodHandler), nil
}

// 使服务端支持HTTP协议
// 接收CONNECT请求 返回了200状态码 HTTP/1.0 200 Connected to Gee RPC

const (
	// 隧道建立成功的响应内容
	connected = "200 Connected to Gee RPC"
	// RPC请求的默认HTTP路径 区分普通HTTP请求
	defaultRPCPath = "/_geerpc_"
	// 调试路径
	defaultDebugPath = "/debug/geerpc"
)

// 实现Handler接口中的ServeHTTP方法 即可处理HTTP请求
func (server *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// 1. 只有连接请求才被允许
	if req.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		// 返回方法不被允许
		_, _ = io.WriteString(w, "405 must CONNECT\n")
		return
	}
	// 2. 劫持HTTP底层TCP连接（关键步骤）
	// http.Hijacker是ResponseWriter的扩展接口，允许“劫持”连接脱离HTTP协议控制
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return
	}
	// 3. 向客户端发送隧道建立成功响应
	_, _ = io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")

	// 4. 将劫持的TCP连接交给RPC服务器处理（后续通信脱离HTTP）
	server.ServeConn(conn)
}

func (server *Server) HandleHTTP() {
	http.Handle(defaultRPCPath, server)
	http.Handle(defaultDebugPath, debugHTTP{server})
	log.Println("rpc server debug path:", defaultDebugPath)
}

func HandleHTTP() {
	DefaultServer.HandleHTTP()
}
