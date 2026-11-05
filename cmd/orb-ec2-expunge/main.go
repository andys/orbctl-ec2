// Command orb-ec2-expunge removes every AMI and EBS snapshot that orb-ec2
// created, and optionally its instances. It lists what it would remove
// unless run with -yes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/andys/orbctl-ec2/internal/orbs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "orb-ec2-expunge:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("orb-ec2-expunge", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), `usage: orb-ec2-expunge [-yes] [-instances] [-provider NAME | -all-providers] [-region R | -all-regions]

Finds the AMIs orb-ec2 created (tagged amp-orb-provider or named amp-orb-*),
their EBS snapshots, and leftover tagged snapshots, and deletes them.
Without -yes it only prints what it would delete.

`)
		flags.PrintDefaults()
	}
	yes := flags.Bool("yes", false, "really delete; without it this is a dry run")
	instances := flags.Bool("instances", false, "also terminate orb and builder instances (their disks go with them)")
	provider := flags.String("provider", os.Getenv("ORB_EC2_PROVIDER"), "only this provider's resources (default $ORB_EC2_PROVIDER, or \"default\")")
	allProviders := flags.Bool("all-providers", false, "resources of every provider name")
	region := flags.String("region", "", "region (default $AWS_REGION, or us-west-2)")
	allRegions := flags.Bool("all-regions", false, "every enabled region")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	o, err := orbs.NewInRegion(ctx, *region)
	if err != nil {
		return err
	}
	opts := orbs.ExpungeOptions{Provider: o.Config().Provider, Instances: *instances, DryRun: !*yes}
	if *provider != "" {
		opts.Provider = *provider
	}
	if *allProviders {
		opts.Provider = ""
	}

	regions := []string{o.Region()}
	if *allRegions {
		if regions, err = o.Regions(ctx); err != nil {
			return err
		}
	}
	total := 0
	for _, r := range regions {
		ro := o
		if r != o.Region() {
			if ro, err = orbs.NewInRegion(ctx, r); err != nil {
				return err
			}
		}
		n, err := ro.Expunge(ctx, opts)
		total += n
		if err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
	}
	who := "provider " + opts.Provider
	if opts.Provider == "" {
		who = "any provider"
	}
	switch {
	case total == 0:
		fmt.Printf("nothing to expunge for %s in %v\n", who, regions)
	case opts.DryRun:
		fmt.Printf("%d resources for %s; run again with -yes to delete them\n", total, who)
	default:
		fmt.Printf("expunged %d resources for %s\n", total, who)
	}
	return nil
}
