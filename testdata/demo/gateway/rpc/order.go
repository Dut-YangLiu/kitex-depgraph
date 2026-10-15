package rpc

import (
	"context"

	"example.com/gateway/kitex_gen/order"
	"example.com/gateway/kitex_gen/order/orderservice"
)

var OrderClient orderservice.Client

func Init() {
	OrderClient = orderservice.MustNewClient("order")
}

func CreateOrder(ctx context.Context, userID int64) (int64, error) {
	resp, err := OrderClient.CreateOrder(ctx, &order.CreateOrderReq{UserId: userID})
	if err != nil {
		return 0, err
	}
	return resp.OrderId, nil
}

func GetOrder(ctx context.Context, orderID int64) string {
	resp, err := OrderClient.GetOrder(ctx, &order.GetOrderReq{OrderId: orderID})
	if err != nil {
		return ""
	}
	return resp.Status
}

func QueryLegacy(ctx context.Context, orderID int64) string {
	_, _ = OrderClient.QueryOrderLegacy(ctx, &order.QueryOrderLegacyReq{OrderId: orderID})
	return "ok"
}
