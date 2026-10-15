package service

import (
	"context"

	"example.com/idl/kitex_gen/user"
	"example.com/idl/kitex_gen/user/userservice"
	"example.com/order-svc/conf"
	"example.com/order-svc/rpc"
)

// Payer is satisfied by rpc.PayAdapter.
type Payer interface {
	Prepay(ctx context.Context, orderID int64) error
}

type OrderService struct {
	userCli userservice.Client
	payer   Payer
}

func NewOrderService() *OrderService {
	return &OrderService{
		userCli: userservice.MustNewClient(conf.UserServiceName),
		payer:   rpc.PayAdapter{},
	}
}

// Create checks the user, creates the order and prepays it.
func (s *OrderService) Create(ctx context.Context, userID int64) (int64, error) {
	if _, err := s.userCli.GetUser(ctx, &user.GetUserReq{Id: userID}); err != nil {
		return 0, err
	}
	orderID := int64(1)
	return orderID, s.payer.Prepay(ctx, orderID)
}

func (s *OrderService) Status(orderID int64) string { return "created" }

// Cancel notifies the users involved in the order.
func (s *OrderService) Cancel(ctx context.Context, orderID int64) error {
	_, err := s.userCli.BatchGetUser(ctx, &user.BatchGetUserReq{Ids: []int64{orderID}})
	return err
}
