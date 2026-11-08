# Builds one FlowGate binary. Pass its package directory from the repository root,
# for example: docker build -f deploy/docker/Dockerfile --build-arg TARGET=services/gateway .
# Both images are pinned by digest; Dependabot proposes updates (.github/dependabot.yml).
# The Go patch release matches the toolchain line of go.mod.
FROM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
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
