// Command user-svc-v2 is a second implementation of the same IDL service,
// registered under a different name. Callers pick it by destService.
package main

import (
	"context"

	user "example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/server"
)

type V2Impl struct{}

func (V2Impl) GetUser(ctx context.Context, req *user.GetUserReq) (*user.GetUserResp, error) {
	return &user.GetUserResp{Name: "v2"}, nil
}

func (V2Impl) BatchGetUser(ctx context.Context, req *user.BatchGetUserReq) (*user.BatchGetUserResp, error) {
	return &user.BatchGetUserResp{}, nil
}

func main() {
	svr := userservice.NewServer(V2Impl{},
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: "user-v2"}))
	_ = svr.Run()
}
