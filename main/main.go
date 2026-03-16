package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"GeeRPC"
	"GeeRPC/codec"
	"GeeRPC/codec/pb"
)

// HelloService --- 1. 定义测试服务 ---
type HelloService struct{}

func (s *HelloService) SayHello(args *pb.HelloArgs, reply *pb.HelloReply) error {
	reply.Message = fmt.Sprintf("Hello, %s! You are %d years old.", args.Name, args.Age)
	return nil
}

func startServer(addr chan string) {
	// 注册服务
	_ = GeeRPC.Register(new(HelloService))

	// 监听本地随机可用端口
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal("network error:", err)
	}
	log.Println("RPC Server is running on", l.Addr().String())

	// 把真实端口传给客户端
	addr <- l.Addr().String()

	// 启动服务器接收请求
	GeeRPC.Accept(l)
}

func main() {
	log.SetFlags(0)
	addr := make(chan string)

	// 启动服务端协程
	go startServer(addr)

	// 获取服务端的地址
	serverAddr := <-addr

	// --- 2. 客户端连接 (强制指定使用 Protobuf 编码) ---
	// 你的 Option 结构体需要支持 CodecType
	client, err := GeeRPC.Dial("tcp", serverAddr, &GeeRPC.Option{
		MagicNumber:    GeeRPC.MagicNumber,
		CodecType:      codec.ProtobufType, // 明确使用刚才写的 ProtobufCodec
		ConnectTimeout: time.Second * 10,
	})
	if err != nil {
		log.Fatal("dial error:", err)
	}
	defer client.Close()

	time.Sleep(time.Second) // 等待服务器就绪

	// --- 3. 疯狂发包测试粘包 (高并发 + 紧凑循环) ---
	var wg sync.WaitGroup
	requestCount := 5000 // 一瞬间发送 5000 个请求

	log.Printf("Start sending %d concurrent requests to test sticky packets...", requestCount)
	startTime := time.Now()

	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// 构造不同的请求大小，更容易诱发粘包
			name := fmt.Sprintf("User_%d", i)
			for j := 0; j < i%5; j++ {
				name += "_padding" // 故意让每个包的大小不一样
			}

			args := &pb.HelloArgs{Name: name, Age: int32(20 + i%50)}
			reply := &pb.HelloReply{}

			// 发起同步调用
			err := client.Call(context.Background(), "HelloService.SayHello", args, reply)
			if err != nil {
				log.Printf("Call error on request %d: %v", i, err)
			} else if i%1000 == 0 {
				// 每 1000 次打印一次进度，证明数据没有乱码
				log.Printf("[Success] %s", reply.Message)
			}
		}(i)
	}

	wg.Wait()
	log.Printf("All %d requests finished in %v. If there are no errors above, sticky packets are solved!", requestCount, time.Since(startTime))
}
