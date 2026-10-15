package handler

import (
	"context"

	"example.com/order-svc/kitex_gen/order"
	"example.com/order-svc/service"
)

// OrderServiceImpl implements order.OrderService.
type OrderServiceImpl struct {
	svc *service.OrderService
}

func New() *OrderServiceImpl {
	return &OrderServiceImpl{svc: service.NewOrderService()}
}

func (h *OrderServiceImpl) CreateOrder(ctx context.Context, req *order.CreateOrderReq) (*order.CreateOrderResp, error) {
	id, err := h.svc.Create(ctx, req.UserId)
	if err != nil {
		return nil, err
	}
	return &order.CreateOrderResp{OrderId: id}, nil
}

func (h *OrderServiceImpl) GetOrder(ctx context.Context, req *order.GetOrderReq) (*order.GetOrderResp, error) {
	return &order.GetOrderResp{Status: h.svc.Status(req.OrderId)}, nil
}

func (h *OrderServiceImpl) CancelOrder(ctx context.Context, req *order.CancelOrderReq) (*order.CancelOrderResp, error) {
	return &order.CancelOrderResp{}, h.svc.Cancel(ctx, req.OrderId)
}
