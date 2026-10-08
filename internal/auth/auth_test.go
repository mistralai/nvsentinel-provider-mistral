// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type reviewFunc func(context.Context, *authv1.TokenReview, metav1.CreateOptions) (*authv1.TokenReview, error)

func (f reviewFunc) Create(ctx context.Context, r *authv1.TokenReview, o metav1.CreateOptions) (*authv1.TokenReview, error) {
	return f(ctx, r, o)
}

func TestAuthenticationAndAuthorization(t *testing.T) {
	const audience = "nvsentinel-csp-provider"
	const subject = "system:serviceaccount:nvsentinel:janitor"
	for _, tc := range []struct {
		name          string
		headers       []string
		authenticated bool
		user          string
		audiences     []string
		reviewErr     error
		want          codes.Code
	}{
		{"valid", []string{"Bearer fake"}, true, subject, []string{audience}, nil, codes.OK},
		{"missing", nil, true, subject, []string{audience}, nil, codes.Unauthenticated},
		{"duplicate", []string{"Bearer fake", "Bearer fake"}, true, subject, []string{audience}, nil, codes.Unauthenticated},
		{"invalid", []string{"Bearer fake"}, false, subject, []string{audience}, nil, codes.Unauthenticated},
		{"wrong audience", []string{"Bearer fake"}, true, subject, []string{"api"}, nil, codes.Unauthenticated},
		{"wrong account", []string{"Bearer fake"}, true, "system:serviceaccount:other:admin", []string{audience}, nil, codes.PermissionDenied},
		{"review failed", []string{"Bearer fake"}, false, "", nil, errors.New("secret marker"), codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			reviewer := reviewFunc(func(ctx context.Context, r *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
				if r.Spec.Token != "fake" || len(r.Spec.Audiences) != 1 || r.Spec.Audiences[0] != audience {
					t.Fatal("incorrect TokenReview request")
				}
				return &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: tc.authenticated, User: authv1.UserInfo{Username: tc.user}, Audiences: tc.audiences}}, tc.reviewErr
			})
			ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"authorization": tc.headers})
			_, err := UnaryInterceptor(reviewer, audience, subject)(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if status.Code(err) != tc.want {
				t.Fatalf("code %s want %s", status.Code(err), tc.want)
			}
			if called != (tc.want == codes.OK) {
				t.Fatal("unauthorized request reached handler")
			}
		})
	}
}
