package awsebs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// ec2Client is the minimal EC2 API surface awsebs needs. Implemented by
// sdkClient (production, backed by aws-sdk-go-v2) and stubbed in tests.
//
// Every method takes ctx and respects ctx.Done(). Async operations
// (attach/detach/expand) are driven to completion by the driver via the
// wait* helpers, which poll getVolume.
//
// The per-call AWS settings (region + optional static credentials) are
// carried on the context via withAWS / awsFromContext, mirroring how the
// do-volume driver stashes its bearer token — so secret rotation takes
// effect on the next reconcile without rebuilding the driver.
type ec2Client interface {
	createVolume(ctx context.Context, in createVolumeIn) (*ebsVolume, error)
	createVolumeFromSnapshot(ctx context.Context, in createVolumeIn, snapshotID string) (*ebsVolume, error)
	getVolume(ctx context.Context, id string) (*ebsVolume, error)
	volumeByTag(ctx context.Context, key, value string) (*ebsVolume, error)
	deleteVolume(ctx context.Context, id string) error
	attachVolume(ctx context.Context, volumeID, instanceID, device string) error
	detachVolume(ctx context.Context, volumeID, instanceID string) error
	modifyVolumeSize(ctx context.Context, volumeID string, sizeGiB int32) error
	createSnapshot(ctx context.Context, volumeID, description string, tags map[string]string) (string, error)
	deleteSnapshot(ctx context.Context, id string) error
	instanceByName(ctx context.Context, name string) (*ec2Instance, error)
}

// ebsVolume mirrors the relevant fields of an EBS volume.
type ebsVolume struct {
	ID          string
	SizeGiB     int32
	State       string // creating | available | in-use | deleting | ...
	Attachments []ebsAttachment
}

// ebsAttachment is one volume->instance attachment.
type ebsAttachment struct {
	InstanceID string
	Device     string
	State      string // attaching | attached | detaching | detached
}

// ec2Instance is the slice of an EC2 instance the driver cares about.
type ec2Instance struct {
	ID      string
	Devices []string // device names already in the instance's block-device mapping
}

// createVolumeIn is the input to createVolume / createVolumeFromSnapshot.
type createVolumeIn struct {
	AvailabilityZone string
	SizeGiB          int32
	VolumeType       string
	Iops             int32 // 0 => omit (driver default)
	Throughput       int32 // 0 => omit
	Encrypted        bool
	KmsKeyID         string
	Tags             map[string]string
}

// awsParams are the per-call AWS settings resolved from StorageClass
// parameters and carried on the context.
type awsParams struct {
	region          string
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
}

type awsCtxKey struct{}

func withAWS(ctx context.Context, p awsParams) context.Context {
	return context.WithValue(ctx, awsCtxKey{}, p)
}

func awsFromContext(ctx context.Context) (awsParams, error) {
	p, _ := ctx.Value(awsCtxKey{}).(awsParams)
	if strings.TrimSpace(p.region) == "" {
		return awsParams{}, errors.New("awsebs: parameters.region is required on the StorageClass")
	}
	return p, nil
}

// errNotFound is returned for the various EC2 "...NotFound" API errors.
// The driver translates it into driver.ErrNotFound / nil at the boundary.
var errNotFound = errors.New("awsebs: aws resource not found")

// isAWSNotFound reports whether err is one of the EC2 not-found API
// error codes (volume / snapshot / instance).
func isAWSNotFound(err error) bool {
	if errors.Is(err, errNotFound) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "InvalidVolume.NotFound", "InvalidSnapshot.NotFound", "InvalidInstanceID.NotFound":
			return true
		}
	}
	return false
}

// sdkClient is the production ec2Client backed by aws-sdk-go-v2. EC2
// clients are built lazily and cached per (region, accessKeyID) so a
// single driver instance can serve StorageClasses spanning regions /
// accounts without rebuilding config (and re-hitting IMDS) every call.
type sdkClient struct {
	mu      sync.Mutex
	clients map[string]*ec2.Client

	// pollInterval governs the driver's attach/detach/expand polling;
	// tests shrink this. Default 2s, applied by the driver via
	// actionPollInterval.
	pollInterval time.Duration

	// loadConfig is overridable in tests; defaults to awscfg.LoadDefaultConfig.
	loadConfig func(ctx context.Context, optFns ...func(*awscfg.LoadOptions) error) (aws.Config, error)
}

func newSDKClient() *sdkClient {
	return &sdkClient{
		clients:      make(map[string]*ec2.Client),
		pollInterval: 2 * time.Second,
		loadConfig:   awscfg.LoadDefaultConfig,
	}
}

func (c *sdkClient) cli(ctx context.Context) (*ec2.Client, error) {
	p, err := awsFromContext(ctx)
	if err != nil {
		return nil, err
	}
	key := p.region + "|" + p.accessKeyID
	c.mu.Lock()
	defer c.mu.Unlock()
	if cli, ok := c.clients[key]; ok {
		return cli, nil
	}
	opts := []func(*awscfg.LoadOptions) error{awscfg.WithRegion(p.region)}
	if p.accessKeyID != "" {
		opts = append(opts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(p.accessKeyID, p.secretAccessKey, p.sessionToken),
		))
	}
	cfg, err := c.loadConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("awsebs: load AWS config (region %s): %w", p.region, err)
	}
	cli := ec2.NewFromConfig(cfg)
	c.clients[key] = cli
	return cli, nil
}

func tagSpecs(rt ec2types.ResourceType, tags map[string]string) []ec2types.TagSpecification {
	if len(tags) == 0 {
		return nil
	}
	t := make([]ec2types.Tag, 0, len(tags))
	for k, v := range tags {
		t = append(t, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return []ec2types.TagSpecification{{ResourceType: rt, Tags: t}}
}

func toEBSVolume(v ec2types.Volume) *ebsVolume {
	out := &ebsVolume{
		ID:    aws.ToString(v.VolumeId),
		State: string(v.State),
	}
	if v.Size != nil {
		out.SizeGiB = *v.Size
	}
	for _, a := range v.Attachments {
		out.Attachments = append(out.Attachments, ebsAttachment{
			InstanceID: aws.ToString(a.InstanceId),
			Device:     aws.ToString(a.Device),
			State:      string(a.State),
		})
	}
	return out
}

func (c *sdkClient) createVolume(ctx context.Context, in createVolumeIn) (*ebsVolume, error) {
	return c.create(ctx, in, "")
}

func (c *sdkClient) createVolumeFromSnapshot(ctx context.Context, in createVolumeIn, snapshotID string) (*ebsVolume, error) {
	return c.create(ctx, in, snapshotID)
}

func (c *sdkClient) create(ctx context.Context, in createVolumeIn, snapshotID string) (*ebsVolume, error) {
	cli, err := c.cli(ctx)
	if err != nil {
		return nil, err
	}
	input := &ec2.CreateVolumeInput{
		AvailabilityZone:  aws.String(in.AvailabilityZone),
		Size:              aws.Int32(in.SizeGiB),
		VolumeType:        ec2types.VolumeType(in.VolumeType),
		Encrypted:         aws.Bool(in.Encrypted),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeVolume, in.Tags),
	}
	if in.Iops > 0 {
		input.Iops = aws.Int32(in.Iops)
	}
	if in.Throughput > 0 {
		input.Throughput = aws.Int32(in.Throughput)
	}
	if in.KmsKeyID != "" {
		input.KmsKeyId = aws.String(in.KmsKeyID)
	}
	if snapshotID != "" {
		input.SnapshotId = aws.String(snapshotID)
	}
	out, err := cli.CreateVolume(ctx, input)
	if err != nil {
		return nil, err
	}
	return &ebsVolume{
		ID:      aws.ToString(out.VolumeId),
		SizeGiB: aws.ToInt32(out.Size),
		State:   string(out.State),
	}, nil
}

func (c *sdkClient) getVolume(ctx context.Context, id string) (*ebsVolume, error) {
	cli, err := c.cli(ctx)
	if err != nil {
		return nil, err
	}
	out, err := cli.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
	if err != nil {
		if isAWSNotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	if len(out.Volumes) == 0 {
		return nil, errNotFound
	}
	return toEBSVolume(out.Volumes[0]), nil
}

func (c *sdkClient) volumeByTag(ctx context.Context, key, value string) (*ebsVolume, error) {
	cli, err := c.cli(ctx)
	if err != nil {
		return nil, err
	}
	out, err := cli.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:" + key), Values: []string{value}}},
	})
	if err != nil {
		return nil, err
	}
	// Ignore volumes that are being deleted — a stale match should not
	// block re-provisioning.
	for i := range out.Volumes {
		v := toEBSVolume(out.Volumes[i])
		if v.State != "deleting" && v.State != "deleted" {
			return v, nil
		}
	}
	return nil, errNotFound
}

func (c *sdkClient) deleteVolume(ctx context.Context, id string) error {
	cli, err := c.cli(ctx)
	if err != nil {
		return err
	}
	_, err = cli.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)})
	if err != nil && isAWSNotFound(err) {
		return errNotFound
	}
	return err
}

func (c *sdkClient) attachVolume(ctx context.Context, volumeID, instanceID, device string) error {
	cli, err := c.cli(ctx)
	if err != nil {
		return err
	}
	_, err = cli.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	})
	return err
}

func (c *sdkClient) detachVolume(ctx context.Context, volumeID, instanceID string) error {
	cli, err := c.cli(ctx)
	if err != nil {
		return err
	}
	in := &ec2.DetachVolumeInput{VolumeId: aws.String(volumeID)}
	if instanceID != "" {
		in.InstanceId = aws.String(instanceID)
	}
	_, err = cli.DetachVolume(ctx, in)
	if err != nil && isAWSNotFound(err) {
		return errNotFound
	}
	return err
}

func (c *sdkClient) modifyVolumeSize(ctx context.Context, volumeID string, sizeGiB int32) error {
	cli, err := c.cli(ctx)
	if err != nil {
		return err
	}
	_, err = cli.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int32(sizeGiB),
	})
	return err
}

func (c *sdkClient) createSnapshot(ctx context.Context, volumeID, description string, tags map[string]string) (string, error) {
	cli, err := c.cli(ctx)
	if err != nil {
		return "", err
	}
	out, err := cli.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId:          aws.String(volumeID),
		Description:       aws.String(description),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeSnapshot, tags),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SnapshotId), nil
}

func (c *sdkClient) deleteSnapshot(ctx context.Context, id string) error {
	cli, err := c.cli(ctx)
	if err != nil {
		return err
	}
	_, err = cli.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(id)})
	if err != nil && isAWSNotFound(err) {
		return errNotFound
	}
	return err
}

// instanceByName resolves a Rune node identity (hostname or NodeID) onto
// an EC2 instance. Resolution order:
//   - "i-..." -> treated as an instance ID directly.
//   - otherwise filter on private-dns-name (the EC2 default hostname,
//     e.g. ip-10-0-1-23.eu-west-2.compute.internal — matched both
//     exactly and as "<name>.*"), falling back to the Name tag.
//
// Returns errNotFound when nothing matches and an error when the match is
// ambiguous (operator must disambiguate via hostname / Name tag).
func (c *sdkClient) instanceByName(ctx context.Context, name string) (*ec2Instance, error) {
	cli, err := c.cli(ctx)
	if err != nil {
		return nil, err
	}
	var input *ec2.DescribeInstancesInput
	if strings.HasPrefix(name, "i-") {
		input = &ec2.DescribeInstancesInput{InstanceIds: []string{name}}
	} else {
		input = &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("private-dns-name"), Values: []string{name, name + ".*"}},
			},
		}
	}
	insts, err := c.describeInstances(ctx, cli, input)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 && !strings.HasPrefix(name, "i-") {
		// Fall back to the Name tag.
		insts, err = c.describeInstances(ctx, cli, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{name}}},
		})
		if err != nil {
			return nil, err
		}
	}
	switch len(insts) {
	case 0:
		return nil, errNotFound
	case 1:
		return insts[0], nil
	default:
		return nil, fmt.Errorf("awsebs: %d EC2 instances match node %q (ambiguous; set a unique hostname or Name tag)", len(insts), name)
	}
}

func (c *sdkClient) describeInstances(ctx context.Context, cli *ec2.Client, in *ec2.DescribeInstancesInput) ([]*ec2Instance, error) {
	out, err := cli.DescribeInstances(ctx, in)
	if err != nil {
		if isAWSNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var insts []*ec2Instance
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			// Skip terminated/terminating instances so a recycled
			// hostname doesn't resolve to a dead box.
			if inst.State != nil {
				switch inst.State.Name {
				case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown:
					continue
				}
			}
			ei := &ec2Instance{ID: aws.ToString(inst.InstanceId)}
			for _, m := range inst.BlockDeviceMappings {
				ei.Devices = append(ei.Devices, aws.ToString(m.DeviceName))
			}
			insts = append(insts, ei)
		}
	}
	return insts, nil
}
