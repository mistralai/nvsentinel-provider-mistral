// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mistralai/nvsentinel-provider-mistral/internal/auth"
	"github.com/mistralai/nvsentinel-provider-mistral/internal/compute"
	"github.com/mistralai/nvsentinel-provider-mistral/internal/config"
	"github.com/mistralai/nvsentinel-provider-mistral/internal/provider"
	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("provider stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	computeClient, err := compute.New(cfg.Compute, nil)
	if err != nil {
		return err
	}
	tlsConfig, err := rotatingTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return err
	}
	kubeConfig, err := kubernetesConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	kubeConfig.Timeout = 15 * time.Second
	rest.AddUserAgent(kubeConfig, "nvsentinel-provider-mistral/"+version)
	kube, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}
	backend, err := provider.New(provider.Config{
		Namespace: cfg.Namespace, NodeNameLabel: cfg.NodeNameLabel, Scope: computeClient.Scope(),
	}, kube, dynamicClient, computeClient)
	if err != nil {
		return err
	}
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.MaxRecvMsgSize(64<<10),
		grpc.MaxConcurrentStreams(64),
		grpc.ChainUnaryInterceptor(
			boundedRPC(cfg.RPCTimeout),
			auth.UnaryInterceptor(kube.AuthenticationV1().TokenReviews(), cfg.Audience, cfg.ServiceAccount),
		),
	)
	cspv1alpha1.RegisterCSPProviderServiceServer(server, backend)
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for gRPC: %w", err)
	}
	defer listener.Close()
	healthListener, err := net.Listen("tcp", cfg.HealthAddress)
	if err != nil {
		return fmt.Errorf("listen for health probes: %w", err)
	}
	defer healthListener.Close()
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	health := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	failures := make(chan error, 2)
	go func() { failures <- server.Serve(listener) }()
	go func() { failures <- health.Serve(healthListener) }()
	ready.Store(true)
	slog.Info("provider started", "version", version, "grpc", cfg.ListenAddress, "namespace", cfg.Namespace)
	select {
	case <-ctx.Done():
	case err = <-failures:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
	}
	ready.Store(false)
	// Drain active RPCs for their full configured deadline before forcing shutdown.
	stopped := make(chan struct{})
	go func() { server.GracefulStop(); close(stopped) }()
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.RPCTimeout+5*time.Second)
	defer cancel()
	select {
	case <-stopped:
	case <-shutdown.Done():
		server.Stop()
	}
	if shutdownErr := health.Shutdown(shutdown); shutdownErr != nil {
		slog.Warn("health server shutdown incomplete")
	}
	return err
}

func kubernetesConfig() (*rest.Config, error) {
	if path := os.Getenv("KUBECONFIG"); path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}

func boundedRPC(timeout time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, req)
	}
}

func rotatingTLS(certFile, keyFile string) (*tls.Config, error) {
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, errors.New("cannot load provider TLS certificate and key")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, errors.New("cannot reload provider TLS certificate and key")
			}
			return &pair, nil
		},
	}, nil
}
