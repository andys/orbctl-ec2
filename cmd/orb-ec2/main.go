// Command orb-ec2 is an Amp orb dispatch program that runs orbs on EC2.
//
// Run it under `amp orb provider` with no arguments: it reads the job from
// ORB_OPERATION and the other ORB_* variables (see
// https://ampcode.com/docs/orbs/self-hosted#dispatch-contract). Each orb is
// an EC2 instance that runs the orb image in Docker. Pause hibernates the
// instance so memory and disk survive, project snapshots are AMIs, and
// `orb-ec2 bake` builds a base AMI with Docker and the orb image preinstalled.
//
// AWS credentials come from the default chain, which on ECS is the task role.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/andys/orbctl-ec2/internal/orbs"
)

const usage = `usage:
  orb-ec2                    run the job described by ORB_OPERATION (dispatch mode)
  orb-ec2 bake [-image IMAGE] [-type ORB_TYPE]
                             build a base AMI with Docker and the orb image
  orb-ec2 list               list orbs and AMIs
  orb-ec2 logs ORB_ID        print the orb instance's console output
  orb-ec2 prune [-older-than 336h] [-dry-run]
                             terminate orbs stopped that long, delete AMIs unused
                             that long and superseded base AMIs
  orb-ec2 serve [-addr :8080]
                             serve an HTML page of orb instances and AMIs, and
                             prune every hour (the address defaults to :$PORT
                             when PORT is set)

Configuration (environment):
  AWS_REGION                 region (default us-west-2)
  ORB_EC2_ROLE_ARN           role to assume on top of the default credentials
  ORB_EC2_PROVIDER           name that scopes this provider's resources (default "default")
  ORB_EC2_TYPES              orb types as name=instance-type,...; the first is the default
  ORB_EC2_SUBNETS            subnet IDs for orbs (default: the subnet of this ECS task)
  ORB_EC2_SECURITY_GROUPS    security group IDs (default: an egress-only group it creates)
  ORB_EC2_VOLUME_GB          root volume size (default 64, raised to fit RAM for hibernation)
  ORB_EC2_PRUNE_AFTER        how long serve keeps unused orbs and AMIs (default 336h; "off" stops pruning)
  ORB_IMAGE                  orb image for bake and spawn (default Amp's x64 image)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orb-ec2:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Print(usage)
		return nil
	}
	timeout := orbs.JobTimeout
	if len(args) > 0 {
		timeout = 2 * time.Hour // admin commands such as bake wait for AMIs
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	o, err := orbs.New(ctx)
	if err != nil {
		return err
	}
	image := os.Getenv("ORB_IMAGE")
	if image == "" {
		image = orbs.DefaultImage
	}

	if len(args) == 0 {
		job := orbs.Job{
			Operation:  os.Getenv("ORB_OPERATION"),
			OrbID:      os.Getenv("ORB_ID"),
			Image:      image,
			SnapshotID: os.Getenv("ORB_SNAPSHOT_ID"),
			Type:       os.Getenv("ORB_TYPE"),
			Token:      os.Getenv("ORB_TOKEN"),
			AmpURL:     os.Getenv("AMP_URL"),
		}
		if job.OrbID == "" {
			return errors.New("ORB_ID is not set; run under `amp orb provider`, or see `orb-ec2 help`")
		}
		return o.Dispatch(ctx, job)
	}

	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "bake":
		flagImage := flags.String("image", image, "orb image to preinstall")
		flagType := flags.String("type", "", "orb type whose instance type builds the AMI (default: the default type)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return o.Bake(ctx, *flagImage, *flagType)
	case "list":
		return o.List(ctx)
	case "logs":
		if len(args) != 2 {
			return errors.New("usage: orb-ec2 logs ORB_ID")
		}
		return o.Logs(ctx, args[1])
	case "prune":
		olderThan := flags.Duration("older-than", cmp.Or(o.Config().PruneAfter, orbs.DefaultPrune), "delete orbs and AMIs not used for this long")
		dryRun := flags.Bool("dry-run", false, "only print what would be deleted")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return o.Prune(ctx, *olderThan, *dryRun)
	case "serve":
		addr := ":8080"
		if port := os.Getenv("PORT"); port != "" {
			addr = ":" + port
		}
		flagAddr := flags.String("addr", addr, "address to listen on")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return o.Serve(*flagAddr)
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}
