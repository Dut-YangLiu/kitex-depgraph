package rpc

import (
	"context"

	"example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
)

var userServiceName = "user"

func GetUserName(ctx context.Context, id int64) string {
	cli := userservice.MustNewClient(userServiceName)
	resp, err := cli.GetUser(ctx, &user.GetUserReq{Id: id})
	if err != nil {
		return ""
	}
	return resp.Name
}
