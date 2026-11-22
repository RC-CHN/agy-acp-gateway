ARG GO_BUILD_IMAGE=golang:1.26-bookworm
FROM --platform=$BUILDPLATFORM ${GO_BUILD_IMAGE} AS go-build
WORKDIR /src
COPY go.mod ./
COPY agy_acp_gateway/ ./agy_acp_gateway/
ENV GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0
RUN go test ./...
ARG TARGETOS TARGETARCH
RUN GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -buildvcs=false -trimpath -o /out/agy-acp-gateway ./agy_acp_gateway

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS frida-wheel
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL --retry 3 https://files.pythonhosted.org/packages/72/b0/154865feab97227423ab1d6e5991f473419da954cba0102a06f4f145e9f0/frida-17.27.4-cp37-abi3-manylinux_2_5_x86_64.whl -o /frida.whl \
    && echo '2d95eb37ff6fd99a8fb3f1f3471d5cfd798f0a5103848c8be797b51ff3d6f6b3  /frida.whl' | sha256sum -c -

FROM debian:bookworm-slim AS agy-fetch
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates bash tar gzip unzip && rm -rf /var/lib/apt/lists/*
ARG AGY_INSTALL_URL=https://antigravity.google/cli/install.sh
RUN set -eu; mkdir -p /out; curl -fsSL "${AGY_INSTALL_URL}" -o /tmp/agy-install.sh; \
    bash /tmp/agy-install.sh --dir /out; \
    install -m 0755 "$(readlink -f /out/agy)" /tmp/agy-elf; rm -f /out/agy; mv /tmp/agy-elf /out/agy
ARG AGY_EXPECTED_VERSION=""
ARG AGY_EXPECTED_SHA256=""
RUN set -eu; test "$(od -An -tx1 -N4 /out/agy | tr -d '[:space:]')" = 7f454c46; \
    version="$(/out/agy --version)"; echo "agy ${version}"; sha256sum /out/agy; \
    if [ -n "${AGY_EXPECTED_VERSION}" ]; then test "${version}" = "${AGY_EXPECTED_VERSION}"; fi; \
    if [ -n "${AGY_EXPECTED_SHA256}" ]; then echo "${AGY_EXPECTED_SHA256}  /out/agy" | sha256sum -c -; fi

FROM debian:bookworm-slim AS runtime-amd64
RUN --mount=type=bind,from=frida-wheel,source=/frida.whl,target=/tmp/frida.whl \
    apt-get update && apt-get install -y --no-install-recommends python3 ca-certificates && rm -rf /var/lib/apt/lists/* \
    && python3 -m zipfile -e /tmp/frida.whl /usr/local/lib/python3.11/dist-packages \
    && python3 -I -c 'import frida; assert frida.__version__ == "17.27.4"'
ENV GATEWAY_STATUS_OBSERVER=frida

FROM debian:bookworm-slim AS runtime-arm64

FROM runtime-${TARGETARCH} AS slim
COPY --from=go-build /out/agy-acp-gateway /usr/local/bin/
COPY LICENSE /usr/share/doc/agy-acp-gateway/LICENSE
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/agy-acp-gateway", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/agy-acp-gateway"]

FROM runtime-${TARGETARCH} AS aio
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && install -d -m 0700 -o 65532 -g 65532 /data /data/.gemini /data/.gemini/antigravity-cli
ENV HOME=/data NO_PROXY=127.0.0.1,localhost,::1 no_proxy=127.0.0.1,localhost,::1
COPY --from=go-build /out/agy-acp-gateway /usr/local/bin/
COPY LICENSE /usr/share/doc/agy-acp-gateway/LICENSE
COPY --from=agy-fetch /out/agy /usr/local/bin/
USER 65532:65532
WORKDIR /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/agy-acp-gateway", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/agy-acp-gateway"]
CMD ["-listen","0.0.0.0:8080","-agy","/usr/local/bin/agy","-live"]
