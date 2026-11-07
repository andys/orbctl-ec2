#!/bin/bash
# amp-orb-agent runs as root on every orb instance (systemd unit amp-orb-agent).
#
# It installs Docker, pulls the orb image, and then waits for spawn jobs. The
# dispatcher has no shell access to the instance, so it posts each job as
# instance tags, which the agent reads through IMDS:
#
#   amp-orb-job        "<nonce>.<chunks>", changed for every job
#   amp-orb-payload-N  base64 chunks of the sealed job payload
#
# The payload holds ORB_TOKEN, sealed to this instance's P-256 key so that
# principals who can read EC2 tags cannot read the token. The agent publishes
# the public key, and reports progress, on the serial console, which the
# dispatcher reads with GetConsoleOutput. Console lines start with "AMP-ORB ".
set -uo pipefail

STATE=/var/lib/amp-orb
IMDS=http://169.254.169.254/latest

say() {
	printf 'AMP-ORB %s\n' "$*" >/dev/console
	printf '%s\n' "$*"
}

imds() {
	local token
	token=$(curl -sf -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "$IMDS/api/token") || return 1
	curl -sf -m 2 -H "X-aws-ec2-metadata-token: $token" "$IMDS/meta-data/$1"
}

# open_payload KEY reads a sealed payload (base64 of ephemeral P-256 public
# key DER, 91 bytes, then a 16-byte IV, then AES-256-CBC ciphertext) on stdin
# and writes the plaintext to stdout. The AES key is SHA-256 of the ECDH secret.
open_payload() {
	local key=$1 dir aes iv rc
	dir=$(mktemp -d) || return 1
	base64 -d >"$dir/blob" &&
		head -c 91 "$dir/blob" >"$dir/peer.der" &&
		openssl pkey -pubin -inform DER -in "$dir/peer.der" -out "$dir/peer.pem" 2>/dev/null &&
		aes=$(openssl pkeyutl -derive -inkey "$key" -peerkey "$dir/peer.pem" | sha256sum | cut -c1-64) &&
		iv=$(tail -c +92 "$dir/blob" | head -c 16 | od -An -tx1 | tr -d ' \n') &&
		tail -c +108 "$dir/blob" | openssl enc -d -aes-256-cbc -K "$aes" -iv "$iv"
	rc=$?
	rm -rf "$dir"
	return $rc
}

ensure_key() {
	local iid=$1
	if [ "$(cat "$STATE/key.iid" 2>/dev/null)" != "$iid" ]; then
		# A new instance id means this disk came from an AMI; never reuse its key.
		openssl ecparam -name prime256v1 -genkey -noout -out "$STATE/key.pem.new" &&
			mv "$STATE/key.pem.new" "$STATE/key.pem" &&
			echo "$iid" >"$STATE/key.iid" || return 1
	fi
	say "PUBKEY $iid $(openssl pkey -in "$STATE/key.pem" -pubout -outform DER | base64 -w0)"
}

apt_install() {
	DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null &&
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null
}

provision() {
	if ! dpkg -s ec2-hibinit-agent >/dev/null 2>&1; then
		say "LOG installing ec2-hibinit-agent"
		apt_install ec2-hibinit-agent || return 1
	fi
	# hibinit-agent sets up the swap file and resume settings that hibernation needs.
	systemctl enable --now hibinit-agent >/dev/null 2>&1 || return 1
	if ! command -v docker >/dev/null; then
		say "LOG installing docker"
		apt_install docker.io || return 1
	fi
	systemctl enable --now docker >/dev/null 2>&1 || return 1
	if ! docker container inspect orb >/dev/null 2>&1 && ! docker image inspect "$ORB_IMAGE" >/dev/null 2>&1; then
		say "LOG pulling $ORB_IMAGE"
		docker pull -q "$ORB_IMAGE" >/dev/null || return 1
	fi
}

run_job() {
	local job=$1 nonce=${1%.*} chunks=${1##*.} blob="" i plain lines
	for ((i = 0; i < chunks; i++)); do
		blob+=$(imds "tags/instance/amp-orb-payload-$i") || return 1
	done
	plain=$(open_payload "$STATE/key.pem" <<<"$blob") || {
		say "LOG cannot open job $nonce"
		ensure_key "$(imds instance-id)"
		return 1
	}
	mapfile -t lines <<<"$plain"
	[ "${lines[0]:-}" = "$nonce" ] || return 1
	if ! docker container inspect orb >/dev/null 2>&1; then
		say "LOG creating orb container"
		# --uts host keeps the hostname current when a snapshot AMI brings this
		# container to a new instance; Docker otherwise fixes it at creation.
		# The image's systemd runs as PID 1, as in Amp's own orbs, so that orb
		# setup scripts can use systemctl (for example to start PostgreSQL).
		# systemd needs tmpfs /run and /run/lock, and stops on SIGRTMIN+3.
		docker run -d --name orb --privileged --network host --uts host --shm-size=1g \
			--cgroupns private --tmpfs /run --tmpfs /run/lock -e container=docker \
			--stop-signal SIGRTMIN+3 --restart unless-stopped "$ORB_IMAGE" /sbin/init >/dev/null || return 1
	fi
	docker start orb >/dev/null || return 1
	# Let systemd finish booting before bootstrap, so that its systemctl calls
	# work. "degraded" means booted with a failed unit. systemctl cannot reach
	# systemd for the first moments, so retry, and bootstrap anyway after 60s.
	local deadline=$((SECONDS + 60))
	while ((SECONDS < deadline)); do
		case $(timeout $((deadline - SECONDS + 1)) docker exec orb systemctl is-system-running --wait 2>/dev/null) in
		running | degraded) break ;;
		esac
		sleep 1
	done
	if [ -n "${lines[2]:-}" ]; then
		ORB_TOKEN=${lines[1]} AMP_URL=${lines[2]} docker exec -d -e ORB_TOKEN -e AMP_URL orb amp orb bootstrap || return 1
	else
		ORB_TOKEN=${lines[1]} docker exec -d -e ORB_TOKEN orb amp orb bootstrap || return 1
	fi
	say "STARTED $nonce"
}

main() {
	# shellcheck source=/dev/null
	. /etc/amp-orb/agent.env
	umask 077
	mkdir -p "$STATE"

	local iid last job
	until iid=$(imds instance-id); do sleep 1; done
	command -v openssl >/dev/null || apt_install openssl
	until ensure_key "$iid"; do sleep 2; done
	until provision; do
		say "LOG provisioning failed, retrying"
		sleep 5
	done
	say "READY $iid"

	last=$(cat "$STATE/last-job" 2>/dev/null || true)
	while :; do
		job=$(imds tags/instance/amp-orb-job || true)
		if [ -n "$job" ] && [ "$job" != "$last" ]; then
			run_job "$job" || say "FAILED ${job%.*}"
			last=$job
			echo "$job" >"$STATE/last-job"
		fi
		sleep 1
	done
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main
fi
