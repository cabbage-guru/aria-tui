FROM alpine:3.19

RUN apk add --no-cache \
    wireguard-tools \
    aria2 \
    iptables \
    ip6tables \
    bash \
    curl \
    jq

# Create directories
RUN mkdir -p /downloads /config

# Copy entrypoint
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 6800

ENTRYPOINT ["/entrypoint.sh"]
