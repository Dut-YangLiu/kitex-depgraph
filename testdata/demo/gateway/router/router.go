// Package router stands in for an HTTP framework router (Hertz, Gin...):
// handlers are registered as function values.
package router

import "example.com/gateway/handler"

type HandlerFunc func(path string) string

type Router struct {
	routes map[string]HandlerFunc
}

func New() *Router { return &Router{routes: map[string]HandlerFunc{}} }

func (r *Router) POST(path string, h HandlerFunc) { r.routes["POST "+path] = h }

func (r *Router) GET(path string, h HandlerFunc) { r.routes["GET "+path] = h }

// Register wires the HTTP API.
func Register(r *Router) {
	r.POST("/order", handler.CreateOrder)
	r.GET("/order", handler.GetOrderDetail)
	r.GET("/order/legacy", handler.LegacyQuery)
	r.GET("/profile", handler.Profile)
}
