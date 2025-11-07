package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DirectRegistration demonstrates how to directly register a container with Rune
// by adding it to the store and then calling StartInstance
func main() {
	// Check command line arguments
	if len(os.Args) != 2 {
		fmt.Println("Usage: register-container <container-id>")
		fmt.Println("Example: register-container abcdef123456")
		os.Exit(1)
	}

	containerID := os.Args[1]

	// Create a logger
	logger := log.NewLogger()

	// Generate a unique instance ID
	instanceID := fmt.Sprintf("inst-%s", uuid.New().String()[:8])

	// Step 1: Create a store and add the instance to it
	storeDir := "./data/store"
	badgerStore := store.NewBadgerStore(logger)
	if err := badgerStore.Open(storeDir); err != nil {
		fmt.Printf("Failed to open store: %v\n", err)
		os.Exit(1)
	}
	defer badgerStore.Close()

	// Create the instance to register
	instance := &types.Instance{
		ID:          instanceID,
		Name:        "test-instance",
		ServiceID:   "test-service",
		ServiceName: "test-service",
		ContainerID: containerID,
		Status:      types.InstanceStatusCreated,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Save the instance to the store
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	namespace := "default"
	if err := badgerStore.Create(ctx, types.ResourceTypeInstance, namespace, instanceID, instance); err != nil {
		fmt.Printf("Failed to save instance to store: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Container registered in store successfully!\n")

	// Step 2: Call StartInstance to activate it
	// Connect to the Rune gRPC server
	conn, err := grpc.Dial("localhost:7863", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Printf("Failed to connect to gRPC server: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Create a client for the InstanceService
	instanceClient := generated.NewInstanceServiceClient(conn)

	// Create an instance action request to start the instance
	startReq := &generated.InstanceActionRequest{
		Id:             instanceID,
		TimeoutSeconds: 30, // Default timeout
	}

	// Call the StartInstance method
	resp, err := instanceClient.StartInstance(ctx, startReq)
	if err != nil {
		fmt.Printf("Warning: Failed to start instance: %v\n", err)
		fmt.Println("Instance is registered in the store but could not be started.")
		fmt.Println("This could be because the container doesn't exist or is not running.")
	} else {
		fmt.Printf("Instance started successfully!\n")
		fmt.Printf("Instance ID: %s\n", resp.Instance.Id)
		fmt.Printf("Container ID: %s\n", resp.Instance.ContainerId)
		fmt.Printf("Status: %s\n", resp.Instance.Status)
	}

	// Show how to use the exec-client with the registered instance
	fmt.Println("\nNow you can use the exec-client with the registered instance ID:")
	fmt.Printf("go run examples/exec-client/main.go %s ls -la\n", instanceID)
}
