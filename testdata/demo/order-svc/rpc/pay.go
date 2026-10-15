package rpc

import (
	"context"

	"example.com/idl/kitex_gen/pay"
	"example.com/idl/kitex_gen/pay/payservice"
	"example.com/order-svc/conf"
)

var payClient payservice.Client

// InitPay creates the payment client. The destination comes from config.
func InitPay(cfg *conf.Config) {
	payClient = payservice.MustNewClient(cfg.PayService)
}

// PayAdapter adapts the payment RPC to service.Payer.
type PayAdapter struct{}

func (PayAdapter) Prepay(ctx context.Context, orderID int64) error {
	_, err := payClient.Prepay(ctx, &pay.PrepayReq{OrderId: orderID})
	return err
}
