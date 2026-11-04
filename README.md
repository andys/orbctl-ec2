# orb-ec2

An [Amp self-hosted orb](https://ampcode.com/docs/orbs/self-hosted) dispatch program that runs each orb on its own EC2 instance.

| Amp job | What orb-ec2 does |
| --- | --- |
| `spawn` | Launches an instance from the project snapshot AMI, the newest base AMI, or stock Amazon Linux 2023, or starts the existing instance, then has it start `amp orb bootstrap` |
| `pause` | Hibernates the instance, so memory and disk survive. With `ORB_SNAPSHOT_ID` set, it stops the instance and saves its disk as an AMI instead |
| `delete` | Terminates the instance, which deletes its disk |

Each instance runs the orb image (`ORB_IMAGE`) in a Docker container named `orb` with host networking. The root volume is encrypted gp3, sized to hold RAM for hibernation. All state lives in EC2 tags, so the dispatcher is stateless.

| | Memory | Disk | Project snapshots |
| --- | --- | --- | --- |
| orb-ec2 | Yes (hibernation) | Yes (EBS) | Yes (AMIs) |

## How the dispatcher reaches the instance

The dispatcher needs only EC2 API access. It needs no SSH, SSM, instance profile, or inbound network path. Instances get an egress-only security group.

1. User data installs `amp-orb-agent` (see `agent.sh`), a systemd service. At boot it creates a P-256 key and prints the public key on the serial console. Then it installs Docker and pulls the image.
2. On the first spawn, the dispatcher reads the key with `GetConsoleOutput` and saves it as the `amp-orb-pubkey` tag.
3. For each spawn, the dispatcher seals `ORB_TOKEN` and `AMP_URL` to that key using ECDH, SHA-256, and AES-256-CBC. It writes the sealed payload to `amp-orb-payload-N` tags and a new nonce to `amp-orb-job`.
4. The agent polls its tags through IMDS, which picks up changes in about 3 seconds. It opens the payload, runs `docker exec -d orb amp orb bootstrap`, and prints `STARTED <nonce>` on the console.

Principals that can read EC2 tags see only the sealed token. Hibernation keeps the agent running, so a resumed orb picks up its job in seconds. EC2 shows console output about 40 seconds late, so only a first spawn waits on the console: once for the key and once for the agent's confirmation. Resumes return as soon as the instance is running.

## Run it

```sh
go build -o /usr/local/bin/orb-ec2 .
orb-ec2 bake                                  # optional but recommended; see below
export AMP_ORB_PROVIDER_TOKEN=aop_live_...    # from Settings → Orbs → Add Orb Provider
amp orb provider /usr/local/bin/orb-ec2
```

Credentials come from the AWS default chain. On ECS, that is the task role. Set `ORB_EC2_ROLE_ARN` to assume another role on top of it.

Without a base AMI, an orb's first boot installs Docker and pulls the 4 GB orb image. That takes about 5 minutes, which is past the provider's limit. The spawn job then returns at the limit, and the agent starts the bootstrap once it is ready. `orb-ec2 bake` builds a base AMI with Docker and the image already in place. Baking takes about 15 minutes, mostly EBS snapshotting. With a base AMI, a first spawn takes about 2 minutes, most of it console delay. A resume from hibernation takes about 8 seconds. Run `bake` again when you want a newer image. Spawn always picks the newest base AMI for the image and architecture.

### Configuration

| Variable | Default |
| --- | --- |
| `AWS_REGION` | `us-west-2` |
| `ORB_EC2_ROLE_ARN` | none: use the default credentials |
| `ORB_EC2_PROVIDER` | `default`. Scopes the tags, so several providers can share an account |
| `ORB_EC2_TYPES` | `medium=m6i.xlarge,small=t3.large,large=m6i.2xlarge`, or Graviton (`m7g`/`t4g`) types when `ORB_IMAGE` is an arm64 image. The first entry is the default. Names match the provider's orb types (`ORB_TYPE`) |
| `ORB_EC2_SUBNETS` | the subnet of the ECS task the dispatcher runs in. The subnet needs outbound internet access |
| `ORB_EC2_SECURITY_GROUPS` | `amp-orbs-<provider>`, created egress-only in the subnet's VPC |
| `ORB_EC2_VOLUME_GB` | `64`, raised to RAM + 32 GiB |
| `ORB_IMAGE` | Amp's x64 orb image. The provider passes it to spawn jobs, and `bake` uses it |

### Admin commands

```sh
orb-ec2 list                     # orbs (with hibernated state) and AMIs
orb-ec2 logs amp-T-…             # console output, including the agent's log
orb-ec2 bake [-image IMG] [-type small]
orb-ec2 prune [-older-than 336h] [-dry-run]   # unused snapshot AMIs, superseded base AMIs
```

## IAM permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:RunInstances", "ec2:StartInstances", "ec2:StopInstances", "ec2:TerminateInstances",
      "ec2:CreateTags", "ec2:DescribeInstances", "ec2:DescribeInstanceTypes", "ec2:GetConsoleOutput",
      "ec2:CreateImage", "ec2:DescribeImages", "ec2:DeregisterImage", "ec2:DeleteSnapshot",
      "ec2:DescribeSubnets", "ec2:DescribeNetworkInterfaces",
      "ec2:DescribeSecurityGroups", "ec2:CreateSecurityGroup"
    ],
    "Resource": "*"
  }]
}
```

Root volumes use the account's default EBS key, `aws/ebs`, which needs no extra KMS permissions.

## Limits

- Hibernation needs the instance to finish preparing its swap file, which takes a few minutes after first boot. A pause before that stops the instance instead, so memory is lost that one time.
- A snapshot pause does a normal stop, because an AMI of a hibernated disk does not boot cleanly. The orb loses memory on that pause. A new thread can use the snapshot once EC2 finishes the AMI. Until then, new threads boot from the image.
- Snapshot AMIs include the orb's hibernation swap file, so they hold RAM-sized data.
