package main

import (
	"context"

	user "example.com/idl/kitex_gen/user"
	"example.com/user-svc/service"
)

// UserServiceImpl implements the last service interface defined in the IDL.
type UserServiceImpl struct{}

// GetUser implements the UserServiceImpl interface.
func (s *UserServiceImpl) GetUser(ctx context.Context, req *user.GetUserReq) (resp *user.GetUserResp, err error) {
	name, err := service.LoadUser(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return &user.GetUserResp{Name: name}, nil
}

// BatchGetUser implements the UserServiceImpl interface.
func (s *UserServiceImpl) BatchGetUser(ctx context.Context, req *user.BatchGetUserReq) (resp *user.BatchGetUserResp, err error) {
	resp = &user.BatchGetUserResp{}
	for _, id := range req.Ids {
		name, err := service.LoadUser(ctx, id)
		if err != nil {
			return nil, err
		}
		resp.Names = append(resp.Names, name)
	}
	return resp, nil
}
