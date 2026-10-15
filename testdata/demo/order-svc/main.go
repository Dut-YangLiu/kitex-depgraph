package main

import (
	"example.com/order-svc/conf"
	"example.com/order-svc/handler"
	"example.com/order-svc/kitex_gen/order"
	"example.com/order-svc/kitex_gen/order/orderservice"
	"example.com/order-svc/rpc"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/server"
)

func main() {
	rpc.InitPay(conf.Get())
	// The handler is held in an interface variable: the scanner finds the
	// implementation with class hierarchy analysis.
	var h order.OrderService = handler.New()
	svr := orderservice.NewServer(h,
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: conf.ServiceName}),
	)
	_ = svr.Run()
}
