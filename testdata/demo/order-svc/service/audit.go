package service

import (
	"context"

	"example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
)

// Audit reads a user from a service chosen at runtime, so the scanner cannot
// tell which implementation of user.UserService is called.
func Audit(ctx context.Context, svcName string, userID int64) {
	cli := userservice.MustNewClient(svcName)
	_, _ = cli.GetUser(ctx, &user.GetUserReq{Id: userID})
}
