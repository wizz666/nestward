# Nestward core – one small container.
# Build:  docker build -t nestward .
# Run:    docker run -d -p 8470:8470 -v nestward-data:/data -v /:/host:ro nestward

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/wizz666/nestward/internal/version.Version=${VERSION}" \
      -o /out/nestward ./cmd/nestward

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/nestward /nestward
ENV NESTWARD_ADDR=0.0.0.0:8470 \
    NESTWARD_DATA=/data \
    NESTWARD_HOST_ROOT=/host
VOLUME ["/data"]
EXPOSE 8470 8472
ENTRYPOINT ["/nestward"]
CMD ["serve"]
