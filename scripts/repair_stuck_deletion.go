// repair_stuck_deletion marks in-progress deletion_operation records as failed.
//
// Usage (runed must be stopped so Badger is not locked; run as the rune user
// or chown -R rune:rune /var/lib/rune/store afterward):
//
//	sudo -u rune go run ./scripts/repair_stuck_deletion.go -store /var/lib/rune/store \
//	  -namespace prod -id delete-prod-gateway-1779294268
//
// Or build for the server:
//
//	GOOS=linux GOARCH=amd64 go build -o repair_stuck_deletion ./scripts/repair_stuck_deletion.go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dgraph-io/badger/v4"
)

type deletionOperation struct {
	ID            string     `json:"id"`
	Namespace     string     `json:"namespace"`
	ServiceName   string     `json:"service_name"`
	Status        string     `json:"status"`
	FailureReason string     `json:"failure_reason,omitempty"`
	StartTime     time.Time  `json:"start_time"`
	EndTime       *time.Time `json:"end_time,omitempty"`
}

func main() {
	storePath := flag.String("store", "/var/lib/rune/store", "Badger store directory")
	namespace := flag.String("namespace", "", "Deletion operation namespace (required)")
	id := flag.String("id", "", "Deletion operation ID (required)")
	reason := flag.String("reason", "manually cleared stuck deletion", "Failure reason to record")
	flag.Parse()

	if *namespace == "" || *id == "" {
		fmt.Fprintln(os.Stderr, "namespace and id are required")
		flag.Usage()
		os.Exit(2)
	}

	key := []byte(fmt.Sprintf("deletion_operation/%s/%s", *namespace, *id))

	db, err := badger.Open(badger.DefaultOptions(*storePath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	var op deletionOperation
	err = db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &op)
		})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", key, err)
		os.Exit(1)
	}

	fmt.Printf("before: status=%s service=%s/%s\n", op.Status, op.Namespace, op.ServiceName)

	switch op.Status {
	case "completed", "failed":
		fmt.Println("already terminal; nothing to do")
		return
	}

	now := time.Now().UTC()
	op.Status = "failed"
	op.FailureReason = *reason
	op.EndTime = &now

	data, err := json.Marshal(&op)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}

	err = db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", key, err)
		os.Exit(1)
	}

	fmt.Printf("after:  status=%s reason=%q\n", op.Status, op.FailureReason)
}
