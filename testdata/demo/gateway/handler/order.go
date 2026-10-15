package handler

import (
	"context"

	"example.com/common-sdk/usersdk"
	"example.com/gateway/rpc"
)

// CreateOrder is POST /order.
func CreateOrder(path string) string {
	id, err := rpc.CreateOrder(context.Background(), 42)
	if err != nil {
		return err.Error()
	}
	return string(rune(id))
}

// GetOrderDetail is GET /order: order status plus the buyer's name.
func GetOrderDetail(path string) string {
	ctx := context.Background()
	return rpc.GetOrder(ctx, 1) + " " + rpc.GetUserName(ctx, 42)
}

// LegacyQuery is GET /order/legacy. It still calls a method the order
// service no longer has.
func LegacyQuery(path string) string {
	return rpc.QueryLegacy(context.Background(), 1)
}

// Profile is GET /profile, served through the shared user SDK.
func Profile(path string) string {
	return usersdk.New().DisplayName(context.Background(), 42)
}
