FROM golang:1.24-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /payback . && mkdir /data

# distroless: CA certificates and a non-root user, nothing else. Time zones are built in.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /payback /payback
COPY --from=build --chown=65532:65532 /data /data
ENV DATA_DIR=/data LISTEN=:8095
VOLUME /data
EXPOSE 8095
HEALTHCHECK --interval=1m --timeout=10s CMD ["/payback", "healthcheck"]
ENTRYPOINT ["/payback"]
