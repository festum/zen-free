FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod *.go ./
ARG OPENCODE_VERSION=
RUN set -eux; \
    V="${OPENCODE_VERSION}"; \
    if [ -z "$V" ]; then \
      V="$(curl -fsSI https://github.com/anomalyco/opencode/releases/latest \
        | tr -d '\r' | sed -n 's|^[Ll]ocation:.*/tag/v||p' | head -n 1)"; \
    fi; \
    LD="-s -w"; \
    if [ -n "$V" ]; then LD="$LD -X main.opencodeVersion=$V"; fi; \
    echo "opencode fingerprint version: ${V:-source default}"; \
    CGO_ENABLED=0 go build -trimpath -ldflags="$LD" -o /out/oc-zen .; \
    if [ -n "$V" ]; then grep -aqF "$V" /out/oc-zen; fi

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/oc-zen /oc-zen
EXPOSE 8801
ENTRYPOINT ["/oc-zen"]
