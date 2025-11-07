package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
)

func main() {
	// Flags
	grpcAddr := flag.String("grpc", ":17863", "gRPC listen address (e.g., :17863)")
	httpAddr := flag.String("http", ":17861", "HTTP listen address (e.g., :17861)")
	withAuth := flag.Bool("auth", false, "Enable token auth (default: disabled)")
	flag.Parse()

	// Quiet logs by default for test runs
	log.SetDefaultLogger(log.NewLogger(log.WithLevel(log.ErrorLevel)))
	logger := log.GetDefaultLogger().WithComponent("runed-test")

	// In-memory store for tests
	mem := store.NewMemoryStore()

	opts := []server.Option{
		server.WithStore(mem),
		server.WithLogger(logger),
		server.WithGRPCAddr(*grpcAddr),
		server.WithHTTPAddr(*httpAddr),
	}
	if *withAuth {
		opts = append(opts, server.WithAuth(nil))
	}

	s, err := server.New(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create server: %v\n", err)
		os.Exit(1)
	}
	if err := s.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start server: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("runed-test started: gRPC=%s, HTTP=%s, auth=%v\n", *grpcAddr, *httpAddr, *withAuth)
	fmt.Println("Press Ctrl+C to stop")

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	_ = s.Stop()
}
