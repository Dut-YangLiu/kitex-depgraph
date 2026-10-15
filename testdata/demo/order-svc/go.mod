module example.com/order-svc

go 1.21

require (
	example.com/idl v0.0.0
	github.com/cloudwego/kitex v0.8.0
)

replace example.com/idl => ../idl
