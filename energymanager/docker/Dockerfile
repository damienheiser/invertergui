# Dockerfile for building GL.iNet GL-SFT1200 firmware with Energy Manager
# Uses Python 2.7 as required by GL.iNet imagebuilder

FROM ubuntu:18.04

ENV DEBIAN_FRONTEND=noninteractive

# Install required packages
RUN apt-get update && apt-get install -y \
    build-essential \
    device-tree-compiler \
    gawk \
    gcc \
    git \
    g++ \
    make \
    ncurses-dev \
    python \
    python-pip \
    unzip \
    wget \
    curl \
    rsync \
    file \
    libncurses5-dev \
    zlib1g-dev \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# Install Go for cross-compiling energymanager
ENV GO_VERSION=1.21.6
RUN wget -q https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz && \
    rm go${GO_VERSION}.linux-amd64.tar.gz

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
ENV PATH=$PATH:$GOPATH/bin

# Create working directories
WORKDIR /build

# Clone GL.iNet imagebuilder
RUN git clone https://github.com/gl-inet/imagebuilder.git /build/imagebuilder
RUN git clone https://github.com/gl-inet/glinet.git /build/glinet

# Entry point
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]
