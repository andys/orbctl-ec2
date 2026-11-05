# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/orb-ec2 ./cmd/orb-ec2

FROM debian:bookworm-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl tini libcap2-bin \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --create-home --uid 1000 orb
COPY --from=build /out/orb-ec2 /usr/local/bin/orb-ec2
# Let the non-root user bind ports below 1024, such as the default PORT 80
RUN setcap cap_net_bind_service=+ep /usr/local/bin/orb-ec2

# The Amp CLI installs into ~/.amp/bin and updates itself there
USER orb
WORKDIR /home/orb
RUN curl -fsSL https://ampcode.com/install.sh | bash
ENV PATH=/home/orb/.amp/bin:/home/orb/.local/bin:$PATH
RUN amp --version

# tini reaps the dispatch processes the provider starts for each job
# orb-ec2 serve answers health checks on $PORT (default 80) and runs the provider

EXPOSE ${PORT}

ENTRYPOINT ["tini", "--", "/usr/local/bin/orb-ec2", "serve"]

# needs AMP_ORB_PROVIDER_TOKEN to be set
