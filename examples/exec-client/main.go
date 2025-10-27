package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/runestack/rune/pkg/api/generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ExecClient demonstrates how to use the bidirectional streaming exec functionality.
func main() {
	// Connect to the Rune gRPC server
	conn, err := grpc.Dial("localhost:8443", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Printf("Failed to connect to server: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Create a client for the ExecService
	client := generated.NewExecServiceClient(conn)

	// Check command line arguments
	if len(os.Args) < 3 {
		fmt.Println("Usage: exec-client <instance-id> <command> [args...]")
		fmt.Println("Example: exec-client instance123 ls -la")
		os.Exit(1)
	}

	instanceID := os.Args[1]
	command := os.Args[2:]

	// Set up context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up interrupt handling
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)
	go func() {
		<-signalCh
		fmt.Println("\nReceived interrupt, closing connection...")
		cancel()
	}()

	// Start the bidirectional stream
	stream, err := client.StreamExec(ctx)
	if err != nil {
		fmt.Printf("Failed to create stream: %v\n", err)
		os.Exit(1)
	}

	// Send initial exec request
	initReq := &generated.ExecRequest{
		Request: &generated.ExecRequest_Init{
			Init: &generated.ExecInitRequest{
				Target: &generated.ExecInitRequest_InstanceId{
					InstanceId: instanceID,
				},
				Command: command,
				Tty:     true, // Enable TTY for interactive sessions
			},
		},
	}

	if err := stream.Send(initReq); err != nil {
		fmt.Printf("Failed to send initial request: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Executing command: %v on instance: %s\n", command, instanceID)

	// Use a separate goroutine to read stdin and send to the server
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			// Send stdin data to the server
			stdinReq := &generated.ExecRequest{
				Request: &generated.ExecRequest_Stdin{
					Stdin: append(scanner.Bytes(), '\n'),
				},
			}

			if err := stream.Send(stdinReq); err != nil {
				fmt.Printf("Failed to send stdin: %v\n", err)
				cancel()
				return
			}
		}

		// If stdin is closed, complete the stream
		cancel()
	}()

	// Receive and process responses from the server
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			// Stream closed by server
			break
		}
		if err != nil {
			fmt.Printf("Error receiving response: %v\n", err)
			break
		}

		// Process response based on type
		switch r := resp.Response.(type) {
		case *generated.ExecResponse_Stdout:
			// Print stdout data
			fmt.Print(string(r.Stdout))

		case *generated.ExecResponse_Stderr:
			// Print stderr data
			fmt.Fprint(os.Stderr, string(r.Stderr))

		case *generated.ExecResponse_Status:
			// Print status message
			if r.Status.Code != 0 {
				fmt.Printf("Status error (code %d): %s\n", r.Status.Code, r.Status.Message)
			}

		case *generated.ExecResponse_Exit:
			// Print exit information
			if r.Exit.Code != 0 {
				fmt.Printf("Command exited with code %d\n", r.Exit.Code)
			}

		default:
			fmt.Printf("Received unknown response type\n")
		}
	}

	fmt.Println("Exec session completed")
}
