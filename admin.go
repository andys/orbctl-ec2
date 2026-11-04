package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Bake builds a base AMI: Amazon Linux 2023 with Docker, the agent, and the
// orb image pulled, so new orbs skip that work on first boot. Spawn picks the
// newest base AMI whose image and architecture match.
func (o *Orbs) Bake(ctx context.Context, image, typeName string) error {
	orbType, err := o.cfg.instanceType(image, typeName)
	if err != nil {
		return err
	}
	info, err := o.instanceTypeInfo(ctx, orbType.InstanceType)
	if err != nil {
		return err
	}
	host, err := o.hostImage(ctx, info.arch)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	inst, err := o.runInstance(ctx, runSpec{
		name:         "amp-orb-builder-" + stamp,
		instanceType: orbType.InstanceType,
		info:         info,
		image:        host,
		orbImage:     image,
		hibernate:    false, // keeps the hibernation swap file out of the AMI
		clientToken:  clientToken(o.cfg.Provider, "bake", stamp),
		tags:         map[string]string{tagKind: "builder", tagImage: image},
	})
	if err != nil {
		return err
	}
	id := aws.ToString(inst.InstanceId)
	logf("launched builder %s (%s) from %s", id, orbType.InstanceType, aws.ToString(host.Name))
	defer func() {
		// Use a fresh context so the builder goes away even after a timeout.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := o.ec2.TerminateInstances(cleanup, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
			logf("could not terminate builder %s: %v", id, err)
		} else {
			logf("terminated builder %s", id)
		}
	}()

	logf("waiting for the builder to install Docker and pull %s", image)
	if err := o.watchConsole(ctx, id, func(lines [][]string) (bool, error) {
		for _, f := range lines {
			if len(f) == 2 && f[0] == "READY" && f[1] == id {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	if err := o.stop(ctx, id, false); err != nil {
		return err
	}
	if _, err := o.waitState(ctx, id, types.InstanceStateNameStopped); err != nil {
		return err
	}
	imageID, err := o.createImage(ctx, id, imageKindBase, fmt.Sprintf("amp-orb-base-%s-%s", info.arch, stamp), map[string]string{
		tagImage: image,
		tagArch:  info.arch,
	})
	if err != nil {
		return err
	}
	logf("saving %s; waiting for it to become available", imageID)
	if err := ec2.NewImageAvailableWaiter(o.ec2).Wait(ctx, &ec2.DescribeImagesInput{ImageIds: []string{imageID}}, time.Until(deadline(ctx))); err != nil {
		return fmt.Errorf("wait for %s: %w", imageID, err)
	}
	logf("base AMI %s is ready for %s on %s", imageID, image, info.arch)
	return nil
}

// List prints this provider's orbs and AMIs.
func (o *Orbs) List(ctx context.Context) error {
	instances, err := o.describeInstances(ctx, filter("instance-state-name", "pending", "running", "stopping", "stopped", "shutting-down"))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORB\tINSTANCE\tSTATE\tTYPE\tZONE\tLAUNCHED")
	for _, inst := range instances {
		state := orbState(inst)
		orb := tagValue(inst.Tags, tagOrbID)
		if orb == "" {
			orb = tagValue(inst.Tags, "Name")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s/%s\t%s\t%s\n", orb, aws.ToString(inst.InstanceId), state,
			tagValue(inst.Tags, tagType), inst.InstanceType, aws.ToString(inst.Placement.AvailabilityZone),
			aws.ToTime(inst.LaunchTime).Format(time.RFC3339))
	}
	images, err := o.describeImages(ctx, []string{"self"}, filter("tag:"+tagProvider, o.cfg.Provider))
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nAMI\tKIND\tSTATE\tARCH\tCREATED\tIMAGE / SNAPSHOT")
	for _, image := range images {
		what := tagValue(image.Tags, tagImage)
		if id := tagValue(image.Tags, tagSnapshotID); id != "" {
			what = id
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", aws.ToString(image.ImageId), tagValue(image.Tags, tagKind),
			image.State, tagValue(image.Tags, tagArch), aws.ToString(image.CreationDate), what)
	}
	return w.Flush()
}

// Logs prints the console output of the orb's instance, which includes the agent's log.
func (o *Orbs) Logs(ctx context.Context, orbID string) error {
	instances, err := o.describeInstances(ctx, filter("tag:"+tagOrbID, orbID))
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return fmt.Errorf("no instance for %s", orbID)
	}
	inst := instances[len(instances)-1]
	out, err := o.ec2.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: inst.InstanceId, Latest: aws.Bool(true)})
	if err != nil {
		return err
	}
	text, _ := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	fmt.Printf("# %s %s (console output lags up to a minute)\n%s", aws.ToString(inst.InstanceId), inst.State.Name, text)
	return nil
}

// Prune deletes what this provider no longer uses. It terminates orbs that
// have been stopped, with their disks, since before olderThan, and builders
// that bake left behind. It deletes snapshot and base AMIs that no spawn has
// used for olderThan, and base AMIs that a newer bake superseded. It only
// touches resources tagged with this provider. Running orbs stay: Amp pauses
// them when their threads go idle.
func (o *Orbs) Prune(ctx context.Context, olderThan time.Duration, dryRun bool) error {
	cutoff := time.Now().Add(-olderThan)
	pruned := 0
	report := func(doing, id, name, reason string) {
		pruned++
		if dryRun {
			doing = "would prune"
		}
		logf("%s %s (%s): %s", doing, id, name, reason)
	}

	instances, err := o.describeInstances(ctx, filter("instance-state-name", "pending", "running", "stopping", "stopped"))
	if err != nil {
		return err
	}
	var terminate []string
	for _, inst := range instances {
		var reason string
		// LaunchTime is the latest start, so a stopped orb has not run since.
		launched := aws.ToTime(inst.LaunchTime)
		switch tagValue(inst.Tags, tagKind) {
		case "orb":
			if inst.State.Name == types.InstanceStateNameStopped && launched.Before(cutoff) {
				reason = "stopped and unused since " + launched.Format(time.RFC3339)
			}
		case "builder":
			if launched.Before(time.Now().Add(-builderMaxAge)) {
				reason = "builder left over from a bake at " + launched.Format(time.RFC3339)
			}
		}
		if reason != "" {
			report("terminating", aws.ToString(inst.InstanceId), tagValue(inst.Tags, "Name"), reason)
			terminate = append(terminate, aws.ToString(inst.InstanceId))
		}
	}
	if len(terminate) > 0 && !dryRun {
		if _, err := o.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: terminate}); err != nil {
			return fmt.Errorf("terminate %s: %w", strings.Join(terminate, ", "), err)
		}
	}

	images, err := o.describeImages(ctx, []string{"self"}, filter("tag:"+tagProvider, o.cfg.Provider), filter("state", "available", "failed"))
	if err != nil {
		return err
	}
	newestBase := map[string]bool{}
	for _, image := range images { // newest first
		var reason string
		used, _ := time.Parse(time.RFC3339, tagValue(image.Tags, tagLastUsed))
		if created, _ := time.Parse(time.RFC3339, aws.ToString(image.CreationDate)); created.After(used) {
			used = created
		}
		switch tagValue(image.Tags, tagKind) {
		case imageKindSnapshot:
			if used.Before(cutoff) {
				reason = "unused since " + used.Format(time.RFC3339)
			}
		case imageKindBase:
			key := tagValue(image.Tags, tagImage) + "|" + tagValue(image.Tags, tagArch)
			if newestBase[key] {
				reason = "superseded base AMI"
			} else if used.Before(cutoff) {
				reason = "unused since " + used.Format(time.RFC3339)
			}
			newestBase[key] = true
		}
		if reason == "" {
			continue
		}
		id := aws.ToString(image.ImageId)
		report("deleting", id, aws.ToString(image.Name), reason)
		if dryRun {
			continue
		}
		if err := o.deleteImage(ctx, image); err != nil {
			return err
		}
	}
	if pruned == 0 {
		logf("nothing to prune")
	}
	return nil
}

// builderMaxAge is how long a bake can run before prune deletes its builder.
const builderMaxAge = 3 * time.Hour

// deleteImage deregisters the AMI and deletes those of its EBS snapshots that
// carry this provider's tag.
func (o *Orbs) deleteImage(ctx context.Context, image types.Image) error {
	id := aws.ToString(image.ImageId)
	var snapshots []string
	for _, bd := range image.BlockDeviceMappings {
		if bd.Ebs != nil && bd.Ebs.SnapshotId != nil {
			snapshots = append(snapshots, aws.ToString(bd.Ebs.SnapshotId))
		}
	}
	var owned []types.Snapshot
	if len(snapshots) > 0 {
		out, err := o.ec2.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
			OwnerIds: []string{"self"},
			Filters:  []types.Filter{filter("snapshot-id", snapshots...), filter("tag:"+tagProvider, o.cfg.Provider)},
		})
		if err != nil {
			return fmt.Errorf("describe snapshots of %s: %w", id, err)
		}
		owned = out.Snapshots
	}
	if _, err := o.ec2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: image.ImageId}); err != nil {
		return fmt.Errorf("deregister %s: %w", id, err)
	}
	for _, snapshot := range owned {
		if _, err := o.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snapshot.SnapshotId}); err != nil {
			return fmt.Errorf("delete snapshot %s of %s: %w", aws.ToString(snapshot.SnapshotId), id, err)
		}
	}
	return nil
}
