package orbs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ownImages returns this account's AMIs that orb-ec2 created: those tagged
// with the provider (any provider when provider is empty) or named amp-orb-*.
func (o *Orbs) ownImages(ctx context.Context, provider string) ([]types.Image, error) {
	byTag := filter("tag-key", tagProvider)
	if provider != "" {
		byTag = filter("tag:"+tagProvider, provider)
	}
	tagged, err := o.describeImages(ctx, []string{"self"}, byTag)
	if err != nil {
		return nil, err
	}
	named, err := o.describeImages(ctx, []string{"self"}, filter("name", "amp-orb-*"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []types.Image
	for _, image := range append(tagged, named...) {
		id := aws.ToString(image.ImageId)
		if seen[id] {
			continue
		}
		if provider != "" && tagValue(image.Tags, tagProvider) != provider && tagValue(image.Tags, tagProvider) != "" {
			continue
		}
		seen[id] = true
		out = append(out, image)
	}
	sort.Slice(out, func(i, j int) bool { return aws.ToString(out[i].CreationDate) > aws.ToString(out[j].CreationDate) })
	return out, nil
}

// ExpungeOptions selects what Expunge removes.
type ExpungeOptions struct {
	Provider  string // only this provider's resources; empty means every provider
	Instances bool   // also terminate orb and builder instances
	DryRun    bool
}

// Expunge deregisters every AMI orb-ec2 created in the region, deletes the
// EBS snapshots behind them and any leftover tagged snapshots, and with
// Instances set terminates the orb and builder instances. It returns how
// many resources it found.
func (o *Orbs) Expunge(ctx context.Context, opts ExpungeOptions) (int, error) {
	found := 0
	act := func(doing, id, detail string) {
		found++
		if opts.DryRun {
			doing = "would " + doing
		}
		logf("%s: %s %s (%s)", o.region, doing, id, detail)
	}

	if opts.Instances {
		filters := []types.Filter{filter("instance-state-name", "pending", "running", "stopping", "stopped")}
		if opts.Provider != "" {
			filters = append(filters, filter("tag:"+tagProvider, opts.Provider))
		} else {
			filters = append(filters, filter("tag-key", tagProvider))
		}
		var ids []string
		pages := ec2.NewDescribeInstancesPaginator(o.ec2, &ec2.DescribeInstancesInput{Filters: filters})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				return found, fmt.Errorf("describe instances: %w", err)
			}
			for _, r := range page.Reservations {
				for _, inst := range r.Instances {
					id := aws.ToString(inst.InstanceId)
					act("terminate", id, tagValue(inst.Tags, "Name"))
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 && !opts.DryRun {
			if _, err := o.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
				return found, fmt.Errorf("terminate %s: %w", strings.Join(ids, ", "), err)
			}
		}
	}

	images, err := o.ownImages(ctx, opts.Provider)
	if err != nil {
		return found, err
	}
	deleted := map[string]bool{}
	for _, image := range images {
		id := aws.ToString(image.ImageId)
		act("deregister", id, fmt.Sprintf("%s, %s, provider %q", aws.ToString(image.Name), image.State, tagValue(image.Tags, tagProvider)))
		var snapshots []string
		for _, bd := range image.BlockDeviceMappings {
			if bd.Ebs != nil && bd.Ebs.SnapshotId != nil {
				snapshots = append(snapshots, aws.ToString(bd.Ebs.SnapshotId))
			}
		}
		if !opts.DryRun {
			if _, err := o.ec2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: image.ImageId}); err != nil {
				return found, fmt.Errorf("deregister %s: %w", id, err)
			}
		}
		for _, snap := range snapshots {
			act("delete snapshot", snap, "of "+id)
			deleted[snap] = true
			if opts.DryRun {
				continue
			}
			if _, err := o.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snap)}); err != nil {
				logf("%s: delete snapshot %s: %v", o.region, snap, err)
			}
		}
	}

	// Snapshots whose AMI is already gone, e.g. after a failed CreateImage.
	byTag := filter("tag-key", tagProvider)
	if opts.Provider != "" {
		byTag = filter("tag:"+tagProvider, opts.Provider)
	}
	pages := ec2.NewDescribeSnapshotsPaginator(o.ec2, &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}, Filters: []types.Filter{byTag}})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return found, fmt.Errorf("describe snapshots: %w", err)
		}
		for _, snap := range page.Snapshots {
			id := aws.ToString(snap.SnapshotId)
			if deleted[id] {
				continue
			}
			act("delete snapshot", id, tagValue(snap.Tags, "Name"))
			if opts.DryRun {
				continue
			}
			if _, err := o.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId}); err != nil {
				logf("%s: delete snapshot %s: %v", o.region, id, err)
			}
		}
	}
	return found, nil
}

// Regions returns the regions enabled for the account.
func (o *Orbs) Regions(ctx context.Context) ([]string, error) {
	out, err := o.ec2.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("describe regions: %w", err)
	}
	var regions []string
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	sort.Strings(regions)
	return regions, nil
}
