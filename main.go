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
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	defaultImage  = "us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest"
	defaultRegion = "us-west-2"
	jobTimeout    = 4*time.Minute + 45*time.Second // the contract allows five minutes
	defaultVolume = 64
	x86OrbTypes   = "medium=m6i.xlarge,small=t3.large,large=m6i.2xlarge"
	gravitonTypes = "medium=m7g.xlarge,small=t4g.large,large=m7g.2xlarge"
	usage         = `usage:
  orb-ec2                    run the job described by ORB_OPERATION (dispatch mode)
  orb-ec2 bake [-image IMAGE] [-type ORB_TYPE]
                             build a base AMI with Docker and the orb image
  orb-ec2 list               list orbs and AMIs
  orb-ec2 logs ORB_ID        print the orb instance's console output
  orb-ec2 prune [-older-than 336h] [-dry-run]
                             delete unused snapshot AMIs and superseded base AMIs

Configuration (environment):
  AWS_REGION                 region (default us-west-2)
  ORB_EC2_ROLE_ARN           role to assume on top of the default credentials
  ORB_EC2_PROVIDER           name that scopes this provider's resources (default "default")
  ORB_EC2_TYPES              orb types as name=instance-type,...; the first is the default
  ORB_EC2_SUBNETS            subnet IDs for orbs (default: the subnet of this ECS task)
  ORB_EC2_SECURITY_GROUPS    security group IDs (default: an egress-only group it creates)
  ORB_EC2_VOLUME_GB          root volume size (default 64, raised to fit RAM for hibernation)
  ORB_IMAGE                  orb image for bake and spawn (default Amp's x64 image)
`
)

type orbType struct{ Name, InstanceType string }

type Config struct {
	Provider       string
	Subnets        []string
	SecurityGroups []string
	Types          string // ORB_EC2_TYPES; empty picks defaults from the image architecture
	VolumeGB       int32
}

// orbTypes returns the configured orb types; the first one is the default.
func (c Config) orbTypes(image string) ([]orbType, error) {
	spec := c.Types
	if spec == "" {
		spec = x86OrbTypes
		if strings.Contains(image, "arm64") {
			spec = gravitonTypes
		}
	}
	var types []orbType
	for _, entry := range strings.Split(spec, ",") {
		name, instanceType, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || name == "" || instanceType == "" {
			return nil, fmt.Errorf("ORB_EC2_TYPES: %q is not name=instance-type", entry)
		}
		types = append(types, orbType{name, instanceType})
	}
	return types, nil
}

func (c Config) instanceType(image, name string) (orbType, error) {
	types, err := c.orbTypes(image)
	if err != nil {
		return orbType{}, err
	}
	if name == "" {
		return types[0], nil
	}
	i := slices.IndexFunc(types, func(t orbType) bool { return t.Name == name })
	if i < 0 {
		names := make([]string, len(types))
		for i, t := range types {
			names[i] = t.Name
		}
		return orbType{}, fmt.Errorf("unknown orb type %q (ORB_EC2_TYPES has %s)", name, strings.Join(names, ", "))
	}
	return types[i], nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loadConfig() (Config, error) {
	c := Config{
		Provider:       os.Getenv("ORB_EC2_PROVIDER"),
		Subnets:        splitList(os.Getenv("ORB_EC2_SUBNETS")),
		SecurityGroups: splitList(os.Getenv("ORB_EC2_SECURITY_GROUPS")),
		Types:          os.Getenv("ORB_EC2_TYPES"),
		VolumeGB:       defaultVolume,
	}
	if c.Provider == "" {
		c.Provider = "default"
	}
	if v := os.Getenv("ORB_EC2_VOLUME_GB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 8 {
			return c, fmt.Errorf("ORB_EC2_VOLUME_GB: %q is not a size in GiB", v)
		}
		c.VolumeGB = int32(n)
	}
	return c, nil
}

func newOrbs(ctx context.Context) (*Orbs, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	if awsCfg.Region == "" {
		awsCfg.Region = defaultRegion
	}
	if arn := os.Getenv("ORB_EC2_ROLE_ARN"); arn != "" {
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsCfg), arn, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "orb-ec2-" + cfg.Provider
		})
		awsCfg.Credentials = aws.NewCredentialsCache(provider)
	}
	return &Orbs{ec2: ec2.NewFromConfig(awsCfg), cfg: cfg}, nil
}

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
	timeout := jobTimeout
	if len(args) > 0 {
		timeout = 2 * time.Hour // admin commands such as bake wait for AMIs
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	orbs, err := newOrbs(ctx)
	if err != nil {
		return err
	}
	image := os.Getenv("ORB_IMAGE")
	if image == "" {
		image = defaultImage
	}

	if len(args) == 0 {
		job := Job{
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
		return orbs.Dispatch(ctx, job)
	}

	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "bake":
		flagImage := flags.String("image", image, "orb image to preinstall")
		flagType := flags.String("type", "", "orb type whose instance type builds the AMI (default: the default type)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return orbs.Bake(ctx, *flagImage, *flagType)
	case "list":
		return orbs.List(ctx)
	case "logs":
		if len(args) != 2 {
			return errors.New("usage: orb-ec2 logs ORB_ID")
		}
		return orbs.Logs(ctx, args[1])
	case "prune":
		olderThan := flags.Duration("older-than", 14*24*time.Hour, "delete snapshot AMIs not used for this long")
		dryRun := flags.Bool("dry-run", false, "only print what would be deleted")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return orbs.Prune(ctx, *olderThan, *dryRun)
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}
