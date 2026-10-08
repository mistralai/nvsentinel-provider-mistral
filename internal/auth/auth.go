// SPDX-License-Identifier: Apache-2.0

// Package auth limits the provider to the configured Janitor ServiceAccount.
package auth

import (
	"context"
	"slices"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Reviewer interface {
	Create(context.Context, *authv1.TokenReview, metav1.CreateOptions) (*authv1.TokenReview, error)
}

func UnaryInterceptor(reviewer Reviewer, audience, subject string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md.Get("authorization")) != 1 {
			return nil, status.Error(codes.Unauthenticated, "one bearer credential is required")
		}
		parts := strings.Fields(md.Get("authorization")[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, status.Error(codes.Unauthenticated, "a bearer credential is required")
		}
		review, err := reviewer.Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{
			Token: parts[1], Audiences: []string{audience},
		}}, metav1.CreateOptions{})
		if err != nil {
			return nil, status.Error(codes.Unavailable, "Kubernetes authentication is unavailable")
		}
		if review == nil || !review.Status.Authenticated || review.Status.Error != "" ||
			!slices.Contains(review.Status.Audiences, audience) {
			return nil, status.Error(codes.Unauthenticated, "credential was not authenticated for this audience")
		}
		if review.Status.User.Username != subject {
			return nil, status.Error(codes.PermissionDenied, "only the configured Janitor ServiceAccount may use this provider")
		}
		return handler(ctx, req)
	}
}
