module example.com/gateway

go 1.21

require (
	example.com/common-sdk v0.0.0
	example.com/idl v0.0.0
	github.com/cloudwego/kitex v0.7.2
)

replace (
	example.com/common-sdk => ../common-sdk
	example.com/idl => ../idl
)
