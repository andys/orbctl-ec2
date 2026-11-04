# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go agent.sh ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/orb-ec2 .

FROM debian:bookworm-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl tini \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --create-home --uid 1000 orb
COPY --from=build /out/orb-ec2 /usr/local/bin/orb-ec2

# The Amp CLI installs into ~/.amp/bin and updates itself there
USER orb
WORKDIR /home/orb
RUN curl -fsSL https://ampcode.com/install.sh | bash
ENV PATH=/home/orb/.local/bin:$PATH

# tini reaps the dispatch processes the provider starts for each job
ENTRYPOINT ["tini", "--", "amp", "orb", "provider", "/usr/local/bin/orb-ec2"]
