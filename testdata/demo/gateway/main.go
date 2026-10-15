package main

import (
	"example.com/gateway/router"
	"example.com/gateway/rpc"
)

func main() {
	rpc.Init()
	router.Register(router.New())
}
