package vip

import (
	"encoding/json"

	"github.com/runestack/rune/pkg/store/orderedlog"
)

// bootstrapOp creates the ClusterNetwork with the given CIDR.
type bootstrapOp struct {
	CIDR string `json:"cidr"`
}

func (o *bootstrapOp) OpType() string           { return OpTypeBootstrapClusterNetwork }
func (o *bootstrapOp) Marshal() ([]byte, error) { return json.Marshal(o) }
func unmarshalBootstrap(b []byte) (orderedlog.Op, error) {
	o := &bootstrapOp{}
	if err := json.Unmarshal(b, o); err != nil {
		return nil, err
	}
	return o, nil
}

// allocateOp allocates a VIP for the given service ID.
type allocateOp struct {
	ServiceID string `json:"serviceID"`
}

func (o *allocateOp) OpType() string           { return OpTypeAllocateVIP }
func (o *allocateOp) Marshal() ([]byte, error) { return json.Marshal(o) }
func unmarshalAllocate(b []byte) (orderedlog.Op, error) {
	o := &allocateOp{}
	if err := json.Unmarshal(b, o); err != nil {
		return nil, err
	}
	return o, nil
}

// releaseOp returns a single IP to the free list and clears the
// owning service's allocation.
type releaseOp struct {
	IP string `json:"ip"`
}

func (o *releaseOp) OpType() string           { return OpTypeReleaseVIP }
func (o *releaseOp) Marshal() ([]byte, error) { return json.Marshal(o) }
func unmarshalRelease(b []byte) (orderedlog.Op, error) {
	o := &releaseOp{}
	if err := json.Unmarshal(b, o); err != nil {
		return nil, err
	}
	return o, nil
}
