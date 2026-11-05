package orbs

import (
	"context"
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
	DefaultImage  = "us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest"
	defaultRegion = "us-west-2"
	JobTimeout    = 4*time.Minute + 45*time.Second // the contract allows five minutes
	defaultVolume = 64
	DefaultPrune  = 14 * 24 * time.Hour
	x86OrbTypes   = "medium=m6i.xlarge,small=t3.large,large=m6i.2xlarge"
	gravitonTypes = "medium=m7g.xlarge,small=t4g.large,large=m7g.2xlarge"
)

type orbType struct{ Name, InstanceType string }

type Config struct {
	Provider       string
	Subnets        []string
	SecurityGroups []string
	Types          string // ORB_EC2_TYPES; empty picks defaults from the image architecture
	VolumeGB       int32
	PruneAfter     time.Duration // 0 when serve does not prune
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
		PruneAfter:     DefaultPrune,
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
	if v := os.Getenv("ORB_EC2_PRUNE_AFTER"); v == "off" {
		c.PruneAfter = 0
	} else if v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("ORB_EC2_PRUNE_AFTER: %q is not a duration such as 336h, or off", v)
		}
		c.PruneAfter = d
	}
	return c, nil
}

// New loads configuration from the environment and connects to EC2.
func New(ctx context.Context) (*Orbs, error) {
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

// Config returns the configuration the orbs were created with.
func (o *Orbs) Config() Config { return o.cfg }
