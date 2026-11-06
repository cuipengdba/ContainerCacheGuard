# Build with --build-arg TARGETARCH=amd64 or arm64 after scripts/build.py.
FROM scratch
ARG TARGETARCH=amd64
COPY dist/containercacheguard-linux-${TARGETARCH} /containercacheguard
USER 65534:65534
ENTRYPOINT ["/containercacheguard"]
CMD ["help"]
