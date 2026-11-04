package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// Tags on instances and AMIs. Instance tags are visible to the agent through
// IMDS, which does not allow "/" in keys.
const (
	tagProvider      = "amp-orb-provider"
	tagKind          = "amp-orb-kind" // orb, builder, base, or snapshot
	tagOrbID         = "amp-orb-id"
	tagType          = "amp-orb-type"
	tagImage         = "amp-orb-image"
	tagArch          = "amp-orb-arch"
	tagSnapshotID    = "amp-orb-snapshot-id"
	tagLastUsed      = "amp-orb-last-used"
	tagPublicKey     = "amp-orb-pubkey"
	tagJob           = "amp-orb-job"
	tagPayloadPrefix = "amp-orb-payload-"

	imageKindBase     = "base"
	imageKindSnapshot = "snapshot"
)

//go:embed agent.sh
var agentScript string

func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: aws.String(name), Values: values}
}

func tagValue(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

func toTags(m map[string]string) []types.Tag {
	tags := make([]types.Tag, 0, len(m))
	for k, v := range m {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return tags
}

func (o *Orbs) tag(ctx context.Context, id string, m map[string]string) error {
	_, err := o.ec2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: toTags(m)})
	if err != nil {
		return fmt.Errorf("tag %s: %w", id, err)
	}
	return nil
}

// describeInstances returns this provider's instances that match the filters.
func (o *Orbs) describeInstances(ctx context.Context, filters ...types.Filter) ([]types.Instance, error) {
	filters = append(filters, filter("tag:"+tagProvider, o.cfg.Provider))
	var out []types.Instance
	pages := ec2.NewDescribeInstancesPaginator(o.ec2, &ec2.DescribeInstancesInput{Filters: filters})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	return out, nil
}

// findInstance returns the orb's live instance, or nil.
func (o *Orbs) findInstance(ctx context.Context, orbID string) (*types.Instance, error) {
	instances, err := o.describeInstances(ctx, filter("tag:"+tagOrbID, orbID),
		filter("instance-state-name", "pending", "running", "stopping", "stopped"))
	if err != nil || len(instances) == 0 {
		return nil, err
	}
	if len(instances) > 1 {
		return nil, fmt.Errorf("%s has %d instances; delete the extra ones", orbID, len(instances))
	}
	return &instances[0], nil
}

// waitState polls until the instance reaches the state.
func (o *Orbs) waitState(ctx context.Context, id string, want types.InstanceStateName) (*types.Instance, error) {
	for {
		out, err := o.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", id, err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return nil, fmt.Errorf("instance %s is gone", id)
		}
		inst := out.Reservations[0].Instances[0]
		switch inst.State.Name {
		case want:
			return &inst, nil
		case types.InstanceStateNameTerminated, types.InstanceStateNameShuttingDown:
			return nil, fmt.Errorf("instance %s is %s", id, inst.State.Name)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s to be %s: %w", id, want, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// consoleLines returns the fields of the agent's lines in the console output.
func (o *Orbs) consoleLines(ctx context.Context, id string) ([][]string, error) {
	out, err := o.ec2.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)})
	if err != nil {
		return nil, fmt.Errorf("console output of %s: %w", id, err)
	}
	text, _ := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	var lines [][]string
	for _, line := range strings.Split(string(text), "\n") {
		if _, rest, ok := strings.Cut(line, "AMP-ORB "); ok {
			lines = append(lines, strings.Fields(rest))
		}
	}
	return lines, nil
}

// watchConsole polls the console until check is done, and prints the
// agent's new progress lines along the way.
func (o *Orbs) watchConsole(ctx context.Context, id string, check func([][]string) (bool, error)) error {
	if o.printed == nil {
		o.printed = map[string]bool{}
	}
	for {
		lines, err := o.consoleLines(ctx, id)
		if err != nil && ctx.Err() == nil {
			return err
		}
		for _, f := range lines {
			if line := strings.Join(f, " "); len(f) > 0 && f[0] != "PUBKEY" && !o.printed[line] {
				o.printed[line] = true
				logf("%s: %s", id, line)
			}
		}
		if done, err := check(lines); done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

// launch creates the orb's instance from its snapshot AMI, the newest base
// AMI for its image, or stock Amazon Linux 2023, in that order.
func (o *Orbs) launch(ctx context.Context, job Job) (*types.Instance, error) {
	orbType, err := o.cfg.instanceType(job.Image, job.Type)
	if err != nil {
		return nil, err
	}
	info, err := o.instanceTypeInfo(ctx, orbType.InstanceType)
	if err != nil {
		return nil, err
	}

	var image *types.Image
	if job.SnapshotID != "" {
		if image, err = o.findSnapshotImage(ctx, job.SnapshotID, true); err != nil {
			return nil, err
		}
		if image != nil && tagValue(image.Tags, tagArch) != info.arch {
			logf("snapshot %s is %s, but orb type %s is %s; not using it", job.SnapshotID, tagValue(image.Tags, tagArch), orbType.Name, info.arch)
			image = nil
		}
		if image != nil {
			_ = o.tag(ctx, aws.ToString(image.ImageId), map[string]string{tagLastUsed: time.Now().UTC().Format(time.RFC3339)})
		} else {
			logf("no saved snapshot %s; booting from %s", job.SnapshotID, job.Image)
		}
	}
	if image == nil {
		if image, err = o.newestImage(ctx, []string{"self"}, filter("tag:"+tagProvider, o.cfg.Provider),
			filter("tag:"+tagKind, imageKindBase), filter("tag:"+tagImage, job.Image), filter("tag:"+tagArch, info.arch)); err != nil {
			return nil, err
		}
	}
	if image == nil {
		logf("no base AMI for %s; installing Docker and pulling the image on first boot takes a few minutes (run `orb-ec2 bake` to skip this)", job.Image)
		if image, err = o.hostImage(ctx, info.arch); err != nil {
			return nil, err
		}
	}

	inst, err := o.runInstance(ctx, runSpec{
		name:         job.OrbID,
		instanceType: orbType.InstanceType,
		info:         info,
		image:        image,
		orbImage:     job.Image,
		hibernate:    true,
		clientToken:  clientToken(o.cfg.Provider, job.OrbID, job.Token),
		tags: map[string]string{
			tagKind:  "orb",
			tagOrbID: job.OrbID,
			tagType:  orbType.Name,
			tagImage: job.Image,
		},
	})
	if err != nil {
		return nil, err
	}
	logf("launched %s (%s, %s) for %s from %s (%s)", aws.ToString(inst.InstanceId), orbType.Name, orbType.InstanceType,
		job.OrbID, aws.ToString(image.ImageId), aws.ToString(image.Name))
	return inst, nil
}

type instanceInfo struct {
	arch      string // x86_64 or arm64, as AMIs name it
	memoryGiB int32
}

func (o *Orbs) instanceTypeInfo(ctx context.Context, instanceType string) (instanceInfo, error) {
	out, err := o.ec2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)}})
	if err != nil {
		return instanceInfo{}, fmt.Errorf("describe instance type %s: %w", instanceType, err)
	}
	if len(out.InstanceTypes) == 0 {
		return instanceInfo{}, fmt.Errorf("unknown instance type %s", instanceType)
	}
	t := out.InstanceTypes[0]
	if !aws.ToBool(t.HibernationSupported) {
		return instanceInfo{}, fmt.Errorf("instance type %s does not support hibernation", instanceType)
	}
	info := instanceInfo{arch: "x86_64", memoryGiB: int32((aws.ToInt64(t.MemoryInfo.SizeInMiB) + 1023) / 1024)}
	if t.ProcessorInfo != nil && len(t.ProcessorInfo.SupportedArchitectures) > 0 &&
		t.ProcessorInfo.SupportedArchitectures[0] == types.ArchitectureTypeArm64 {
		info.arch = "arm64"
	}
	return info, nil
}

type runSpec struct {
	name, instanceType string
	info               instanceInfo
	image              *types.Image
	orbImage           string
	hibernate          bool
	clientToken        string
	tags               map[string]string
}

func (o *Orbs) runInstance(ctx context.Context, spec runSpec) (*types.Instance, error) {
	subnets, securityGroups, err := o.network(ctx)
	if err != nil {
		return nil, err
	}
	// Hibernation writes RAM to the root volume.
	size := max(o.cfg.VolumeGB, spec.info.memoryGiB+32)
	for _, bd := range spec.image.BlockDeviceMappings {
		if aws.ToString(bd.DeviceName) == aws.ToString(spec.image.RootDeviceName) && bd.Ebs != nil {
			size = max(size, aws.ToInt32(bd.Ebs.VolumeSize))
		}
	}
	tags := map[string]string{"Name": spec.name, tagProvider: o.cfg.Provider}
	for k, v := range spec.tags {
		tags[k] = v
	}

	// Spread orbs over the subnets and move on when one has no capacity.
	h := fnv.New32a()
	h.Write([]byte(spec.name))
	start := int(h.Sum32() % uint32(len(subnets)))
	var lastErr error
	for i := range subnets {
		subnet := subnets[(start+i)%len(subnets)]
		out, err := o.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
			ImageId:          spec.image.ImageId,
			InstanceType:     types.InstanceType(spec.instanceType),
			MinCount:         aws.Int32(1),
			MaxCount:         aws.Int32(1),
			ClientToken:      aws.String(fmt.Sprintf("%s-%d", spec.clientToken, i)),
			SubnetId:         aws.String(subnet),
			SecurityGroupIds: securityGroups,
			UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(userData(spec.orbImage)))),
			HibernationOptions: &types.HibernationOptionsRequest{
				Configured: aws.Bool(spec.hibernate),
			},
			BlockDeviceMappings: []types.BlockDeviceMapping{{
				DeviceName: spec.image.RootDeviceName,
				Ebs: &types.EbsBlockDevice{
					VolumeSize:          aws.Int32(size),
					VolumeType:          types.VolumeTypeGp3,
					Encrypted:           aws.Bool(true), // required for hibernation
					DeleteOnTermination: aws.Bool(true),
				},
			}},
			MetadataOptions: &types.InstanceMetadataOptionsRequest{
				HttpTokens:           types.HttpTokensStateRequired,
				HttpEndpoint:         types.InstanceMetadataEndpointStateEnabled,
				InstanceMetadataTags: types.InstanceMetadataTagsStateEnabled,
			},
			TagSpecifications: []types.TagSpecification{
				{ResourceType: types.ResourceTypeInstance, Tags: toTags(tags)},
				{ResourceType: types.ResourceTypeVolume, Tags: toTags(tags)},
			},
		})
		if err == nil {
			return &out.Instances[0], nil
		}
		lastErr = fmt.Errorf("run instance in %s: %w", subnet, err)
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || !capacityError(apiErr.ErrorCode(), apiErr.ErrorMessage()) {
			return nil, lastErr
		}
		logf("%v; trying the next subnet", lastErr)
	}
	return nil, lastErr
}

func capacityError(code, message string) bool {
	return code == "InsufficientInstanceCapacity" || code == "InsufficientHostCapacity" ||
		(code == "Unsupported" && strings.Contains(message, "Availability Zone"))
}

// clientToken makes RunInstances idempotent when the same job runs twice.
func clientToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:48]
}

// userData installs and starts the agent. cloud-init runs it once per
// instance, including instances launched from base and snapshot AMIs.
func userData(orbImage string) string {
	return "#!/bin/bash\nset -eu\nmkdir -p /etc/amp-orb\n" +
		"cat >/etc/amp-orb/agent.env <<'EOF'\nORB_IMAGE=" + shellQuote(orbImage) + "\nEOF\n" +
		"cat >/usr/local/sbin/amp-orb-agent <<'AMP_ORB_AGENT_EOF'\n" + agentScript + "AMP_ORB_AGENT_EOF\n" +
		"chmod 755 /usr/local/sbin/amp-orb-agent\n" +
		`cat >/etc/systemd/system/amp-orb-agent.service <<'EOF'
[Unit]
Description=Amp orb agent
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/sbin/amp-orb-agent
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable amp-orb-agent
systemctl restart amp-orb-agent
`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// network returns the subnets and security groups for orbs. By default it
// uses the subnet of the ECS task it runs in and an egress-only security
// group: the agent needs no inbound connections.
func (o *Orbs) network(ctx context.Context) ([]string, []string, error) {
	if o.subnets != nil {
		return o.subnets, o.securityGroups, nil
	}
	subnets := o.cfg.Subnets
	if len(subnets) == 0 {
		subnet, err := o.taskSubnet(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("set ORB_EC2_SUBNETS: %w", err)
		}
		subnets = []string{subnet}
	}
	securityGroups := o.cfg.SecurityGroups
	if len(securityGroups) == 0 {
		group, err := o.ensureSecurityGroup(ctx, subnets[0])
		if err != nil {
			return nil, nil, err
		}
		securityGroups = []string{group}
	}
	o.subnets, o.securityGroups = subnets, securityGroups
	return subnets, securityGroups, nil
}

func (o *Orbs) taskSubnet(ctx context.Context) (string, error) {
	uri := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if uri == "" {
		return "", errors.New("not running on ECS, so there is no task subnet to default to")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri+"/task", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ECS task metadata: %w", err)
	}
	defer resp.Body.Close()
	var task struct {
		Containers []struct {
			Networks []struct{ MACAddress string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return "", fmt.Errorf("ECS task metadata: %w", err)
	}
	for _, c := range task.Containers {
		for _, n := range c.Networks {
			if n.MACAddress == "" {
				continue
			}
			out, err := o.ec2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
				Filters: []types.Filter{filter("mac-address", n.MACAddress)},
			})
			if err != nil {
				return "", fmt.Errorf("find the task's network interface: %w", err)
			}
			if len(out.NetworkInterfaces) > 0 {
				return aws.ToString(out.NetworkInterfaces[0].SubnetId), nil
			}
		}
	}
	return "", errors.New("ECS task metadata has no awsvpc network interface")
}

func (o *Orbs) ensureSecurityGroup(ctx context.Context, subnet string) (string, error) {
	subnets, err := o.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnet}})
	if err != nil || len(subnets.Subnets) == 0 {
		return "", fmt.Errorf("describe subnet %s: %w", subnet, err)
	}
	vpc := aws.ToString(subnets.Subnets[0].VpcId)
	name := "amp-orbs-" + o.cfg.Provider
	find := func() (string, error) {
		out, err := o.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			Filters: []types.Filter{filter("vpc-id", vpc), filter("group-name", name)},
		})
		if err != nil {
			return "", fmt.Errorf("describe security groups: %w", err)
		}
		if len(out.SecurityGroups) == 0 {
			return "", nil
		}
		return aws.ToString(out.SecurityGroups[0].GroupId), nil
	}
	if id, err := find(); id != "" || err != nil {
		return id, err
	}
	out, err := o.ec2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String("Amp orbs: outbound only"),
		VpcId:       aws.String(vpc),
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeSecurityGroup,
			Tags:         toTags(map[string]string{"Name": name, tagProvider: o.cfg.Provider}),
		}},
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidGroup.Duplicate" {
		return find()
	}
	if err != nil {
		return "", fmt.Errorf("create security group %s: %w", name, err)
	}
	logf("created security group %s (%s) in %s", name, aws.ToString(out.GroupId), vpc)
	return aws.ToString(out.GroupId), nil
}

// describeImages returns images that match, newest first.
func (o *Orbs) describeImages(ctx context.Context, owners []string, filters ...types.Filter) ([]types.Image, error) {
	var images []types.Image
	pages := ec2.NewDescribeImagesPaginator(o.ec2, &ec2.DescribeImagesInput{Owners: owners, Filters: filters})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe images: %w", err)
		}
		images = append(images, page.Images...)
	}
	sort.Slice(images, func(i, j int) bool {
		return aws.ToString(images[i].CreationDate) > aws.ToString(images[j].CreationDate)
	})
	return images, nil
}

func (o *Orbs) newestImage(ctx context.Context, owners []string, filters ...types.Filter) (*types.Image, error) {
	images, err := o.describeImages(ctx, owners, append(filters, filter("state", "available"))...)
	if err != nil || len(images) == 0 {
		return nil, err
	}
	return &images[0], nil
}

// hostImage returns the newest stock Amazon Linux 2023 AMI, which has the
// hibernation agent preinstalled.
func (o *Orbs) hostImage(ctx context.Context, arch string) (*types.Image, error) {
	image, err := o.newestImage(ctx, []string{"amazon"}, filter("name", "al2023-ami-2023.*-kernel-*-"+arch),
		filter("architecture", arch))
	if err == nil && image == nil {
		err = fmt.Errorf("no Amazon Linux 2023 AMI for %s", arch)
	}
	return image, err
}

// findSnapshotImage returns the AMI saved for a project snapshot, or nil.
// With availableOnly unset it also returns an AMI that is still being saved.
func (o *Orbs) findSnapshotImage(ctx context.Context, snapshotID string, availableOnly bool) (*types.Image, error) {
	states := []string{"available"}
	if !availableOnly {
		states = append(states, "pending")
	}
	images, err := o.describeImages(ctx, []string{"self"}, filter("tag:"+tagProvider, o.cfg.Provider),
		filter("tag:"+tagKind, imageKindSnapshot), filter("tag:"+tagSnapshotID, snapshotID), filter("state", states...))
	if err != nil || len(images) == 0 {
		return nil, err
	}
	return &images[0], nil
}

var unsafeNameChars = regexp.MustCompile(`[^a-zA-Z0-9().\-/_]+`)

// snapshotImageName is a valid, unique AMI name for a project snapshot.
func snapshotImageName(snapshotID string) string {
	readable := unsafeNameChars.ReplaceAllString(snapshotID, "-")
	if len(readable) > 64 {
		readable = readable[:64]
	}
	return "amp-orb-snapshot-" + readable + "-" + clientToken(snapshotID)[:12]
}

// createImage saves a stopped instance's disk as an AMI and returns its ID
// without waiting for EC2 to finish copying the snapshot.
func (o *Orbs) createImage(ctx context.Context, instanceID, kind, name string, extraTags map[string]string) (string, error) {
	tags := map[string]string{"Name": name, tagProvider: o.cfg.Provider, tagKind: kind}
	for k, v := range extraTags {
		tags[k] = v
	}
	out, err := o.ec2.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId:  aws.String(instanceID),
		Name:        aws.String(name),
		Description: aws.String("Amp orb " + kind + " image"),
		NoReboot:    aws.Bool(true), // the instance is already stopped
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeImage, Tags: toTags(tags)},
			{ResourceType: types.ResourceTypeSnapshot, Tags: toTags(tags)},
		},
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidAMIName.Duplicate" {
		images, derr := o.describeImages(ctx, []string{"self"}, filter("name", name))
		if derr == nil && len(images) > 0 {
			return aws.ToString(images[0].ImageId), nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("create image of %s: %w", instanceID, err)
	}
	return aws.ToString(out.ImageId), nil
}
