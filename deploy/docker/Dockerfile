# Builds one FlowGate binary. Pass its package directory from the repository root,
# for example: docker build -f deploy/docker/Dockerfile --build-arg TARGET=services/gateway .
# Both images are pinned by digest; Dependabot proposes updates (.github/dependabot.yml).
# The Go patch release matches the toolchain line of go.mod.
FROM golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG TARGET
RUN test -n "$TARGET" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./$TARGET"

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/app /app
# The image already defaults to nonroot; stating it lets scanners and readers see it.
USER 65532:65532
ENTRYPOINT ["/app"]
