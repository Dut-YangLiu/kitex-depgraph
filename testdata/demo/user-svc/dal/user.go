package dal

import "context"

type User struct {
	ID   int64
	Name string
}

// QueryUser reads a user from the database.
func QueryUser(ctx context.Context, id int64) (*User, error) {
	return &User{ID: id, Name: "demo"}, nil
}
