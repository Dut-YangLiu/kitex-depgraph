// Package usersdk is a shared client library wrapping the user service.
// Its RPC calls belong to whichever service calls into it.
package usersdk

import (
	"context"

	"example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
)

type Client struct {
	cli userservice.Client
}

func New() *Client {
	return &Client{cli: userservice.MustNewClient("user")}
}

func (c *Client) DisplayName(ctx context.Context, id int64) string {
	resp, err := c.cli.GetUser(ctx, &user.GetUserReq{Id: id})
	if err != nil {
		return ""
	}
	return resp.Name
}

// Names is not used by any service in the workspace.
func (c *Client) Names(ctx context.Context, ids []int64) []string {
	resp, err := c.cli.BatchGetUser(ctx, &user.BatchGetUserReq{Ids: ids})
	if err != nil {
		return nil
	}
	return resp.Names
}
