package service

import (
	"context"

	"example.com/user-svc/dal"
)

// LoadUser returns the display name of a user.
func LoadUser(ctx context.Context, id int64) (string, error) {
	u, err := dal.QueryUser(ctx, id)
	if err != nil {
		return "", err
	}
	return u.Name, nil
}
