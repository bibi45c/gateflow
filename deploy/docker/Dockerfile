# Builds one FlowGate binary. Pass its package directory from the repository root,
# for example: docker build -f deploy/docker/Dockerfile --build-arg TARGET=services/gateway .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG TARGET
RUN test -n "$TARGET" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./$TARGET"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
