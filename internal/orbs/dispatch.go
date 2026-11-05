package orbs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Job is one dispatch job from `amp orb provider`.
type Job struct {
	Operation  string // spawn, pause, or delete
	OrbID      string
	Image      string
	SnapshotID string
	Type       string
	Token      string
	AmpURL     string
}

type Orbs struct {
	ec2    *ec2.Client
	cfg    Config
	region string

	subnets, securityGroups []string        // resolved lazily by network()
	printed                 map[string]bool // console lines already logged
}

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func (o *Orbs) Dispatch(ctx context.Context, job Job) error {
	switch job.Operation {
	case "spawn":
		if job.Token == "" {
			return errors.New("spawn: ORB_TOKEN is not set")
		}
		return o.spawn(ctx, job)
	case "pause":
		return o.pause(ctx, job)
	case "delete":
		return o.delete(ctx, job)
	}
	return fmt.Errorf("unknown ORB_OPERATION %q", job.Operation)
}

// spawn makes the orb's instance run and starts `amp orb bootstrap` in it.
//
// The token reaches the instance as tags sealed to the instance's key. A new
// instance first has to publish that key on its console, which EC2 shows with
// a delay of up to a minute, so a first spawn waits for the key and then for
// the agent to confirm the bootstrap. A resume already knows the key: it posts
// the job before starting the instance and returns once the instance runs.
func (o *Orbs) spawn(ctx context.Context, job Job) error {
	inst, err := o.findInstance(ctx, job.OrbID)
	if err != nil {
		return err
	}
	if inst == nil {
		if inst, err = o.launch(ctx, job); err != nil {
			return err
		}
	}
	id := aws.ToString(inst.InstanceId)
	state := inst.State.Name
	if state == types.InstanceStateNameStopping {
		logf("waiting for %s to finish stopping", id)
		if inst, err = o.waitState(ctx, id, types.InstanceStateNameStopped); err != nil {
			return err
		}
		state = inst.State.Name
	}

	publicKey := tagValue(inst.Tags, tagPublicKey)
	if publicKey != "" {
		if _, err := o.postJob(ctx, id, publicKey, job); err != nil {
			return err
		}
	}
	if state == types.InstanceStateNameStopped {
		logf("starting %s", id)
		if _, err := o.ec2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return fmt.Errorf("start %s: %w", id, err)
		}
	}
	if publicKey != "" {
		if _, err := o.waitState(ctx, id, types.InstanceStateNameRunning); err != nil {
			return err
		}
		logf("%s is running; its agent starts amp orb bootstrap", id)
		return nil
	}

	logf("waiting for %s to publish its key on the console", id)
	if err := o.watchConsole(ctx, id, func(lines [][]string) (bool, error) {
		for _, f := range lines { // the newest key wins
			if len(f) == 3 && f[0] == "PUBKEY" && f[1] == id {
				publicKey = f[2]
			}
		}
		return publicKey != "", nil
	}); err != nil {
		return err
	}
	if err := o.tag(ctx, id, map[string]string{tagPublicKey: publicKey}); err != nil {
		return err
	}
	nonce, err := o.postJob(ctx, id, publicKey, job)
	if err != nil {
		return err
	}

	logf("waiting for the agent on %s to start amp orb bootstrap", id)
	// Leave time to report before the provider's five-minute limit.
	waitCtx, cancel := context.WithTimeout(ctx, time.Until(deadline(ctx))-15*time.Second)
	defer cancel()
	err = o.watchConsole(waitCtx, id, func(lines [][]string) (bool, error) {
		for _, f := range lines {
			if len(f) == 2 && f[1] == nonce {
				switch f[0] {
				case "STARTED":
					return true, nil
				case "FAILED":
					return true, fmt.Errorf("the agent on %s could not start amp orb bootstrap; see `orb-ec2 logs %s`", id, job.OrbID)
				}
			}
		}
		return false, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		logf("%s is still preparing; its agent starts amp orb bootstrap when it is ready", id)
		return nil
	}
	if err == nil {
		logf("started amp orb bootstrap on %s", id)
	}
	return err
}

// postJob seals the token to the instance's key and posts it as tags for the agent.
func (o *Orbs) postJob(ctx context.Context, id, publicKey string, job Job) (string, error) {
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(nonceBytes)
	sealed, err := seal(publicKey, []byte(nonce+"\n"+job.Token+"\n"+job.AmpURL+"\n"))
	if err != nil {
		return "", err
	}
	payload := map[string]string{}
	chunks := 0
	for ; len(sealed) > 0; chunks++ {
		n := min(len(sealed), 256) // the tag value limit
		payload[fmt.Sprintf("%s%d", tagPayloadPrefix, chunks)] = sealed[:n]
		sealed = sealed[n:]
	}
	// Post the payload before the job tag that points at it.
	if err := o.tag(ctx, id, payload); err != nil {
		return "", err
	}
	if err := o.tag(ctx, id, map[string]string{tagJob: fmt.Sprintf("%s.%d", nonce, chunks)}); err != nil {
		return "", err
	}
	return nonce, nil
}

// pause stops the instance and keeps its disk. A plain pause hibernates, so
// memory survives too. A snapshot pause stops the instance normally, because
// an AMI of a hibernated disk does not boot cleanly, and saves the disk as an
// AMI tagged with ORB_SNAPSHOT_ID.
func (o *Orbs) pause(ctx context.Context, job Job) error {
	inst, err := o.findInstance(ctx, job.OrbID)
	if err != nil {
		return err
	}
	if inst == nil {
		logf("no instance for %s; nothing to pause", job.OrbID)
		return nil
	}
	id := aws.ToString(inst.InstanceId)
	if inst.State.Name == types.InstanceStateNamePending {
		if inst, err = o.waitState(ctx, id, types.InstanceStateNameRunning); err != nil {
			return err
		}
	}

	snapshot := job.SnapshotID != ""
	if snapshot {
		image, err := o.findSnapshotImage(ctx, job.SnapshotID, false)
		if err != nil {
			return err
		}
		if image != nil {
			logf("snapshot %s is already saved as %s", job.SnapshotID, aws.ToString(image.ImageId))
			snapshot = false
		}
	}

	if snapshot && hibernated(inst) {
		// Wake it so the disk can be saved without a hibernation image.
		logf("resuming %s to save a snapshot", id)
		if _, err := o.ec2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return fmt.Errorf("start %s: %w", id, err)
		}
		if inst, err = o.waitState(ctx, id, types.InstanceStateNameRunning); err != nil {
			return err
		}
	}
	if inst.State.Name == types.InstanceStateNameRunning {
		if err := o.stop(ctx, id, !snapshot); err != nil {
			return err
		}
	}
	if _, err := o.waitState(ctx, id, types.InstanceStateNameStopped); err != nil {
		return err
	}
	if !snapshot {
		logf("paused %s", id)
		return nil
	}
	imageID, err := o.createImage(ctx, id, imageKindSnapshot, snapshotImageName(job.SnapshotID), map[string]string{
		tagSnapshotID: job.SnapshotID,
		tagImage:      tagValue(inst.Tags, tagImage),
		tagArch:       string(inst.Architecture),
		tagLastUsed:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	logf("paused %s and saved snapshot %s as %s", id, job.SnapshotID, imageID)
	return nil
}

// stop hibernates the instance when asked and possible, otherwise stops it.
func (o *Orbs) stop(ctx context.Context, id string, hibernate bool) error {
	if hibernate {
		_, err := o.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}, Hibernate: aws.Bool(true)})
		if err == nil {
			logf("hibernating %s", id)
			return nil
		}
		// Hibernation is not ready for the first minutes after launch, while
		// the instance prepares its swap file.
		if !strings.Contains(strings.ToLower(err.Error()), "hibernat") {
			return fmt.Errorf("hibernate %s: %w", id, err)
		}
		logf("cannot hibernate %s yet, stopping it instead: %v", id, err)
	}
	if _, err := o.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
		return fmt.Errorf("stop %s: %w", id, err)
	}
	logf("stopping %s", id)
	return nil
}

// delete terminates the orb's instances, which deletes their disks.
func (o *Orbs) delete(ctx context.Context, job Job) error {
	instances, err := o.describeInstances(ctx, filter("tag:"+tagOrbID, job.OrbID),
		filter("instance-state-name", "pending", "running", "stopping", "stopped"))
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		logf("no instance for %s; nothing to delete", job.OrbID)
		return nil
	}
	ids := make([]string, len(instances))
	for i, inst := range instances {
		ids[i] = aws.ToString(inst.InstanceId)
	}
	if _, err := o.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		return fmt.Errorf("terminate %s: %w", strings.Join(ids, ", "), err)
	}
	logf("terminated %s", strings.Join(ids, ", "))
	return nil
}

func hibernated(inst *types.Instance) bool {
	return inst.State.Name == types.InstanceStateNameStopped && inst.StateReason != nil &&
		aws.ToString(inst.StateReason.Code) == "Client.UserInitiatedHibernate"
}

func deadline(ctx context.Context) time.Time {
	d, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(JobTimeout)
	}
	return d
}
