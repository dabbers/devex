package main

import "context"

// The person the platform says is making this request.

type userKey struct{}

type currentUser struct {
	ID    string
	Email string
}

func withUser(ctx context.Context, id, email string) context.Context {
	return context.WithValue(ctx, userKey{}, currentUser{ID: id, Email: email})
}

func userFrom(ctx context.Context) currentUser {
	user, _ := ctx.Value(userKey{}).(currentUser)
	return user
}
