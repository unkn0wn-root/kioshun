FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    cd _benchmarks; \
    go mod tidy; \
    cd cluster/runner && go build -o /out/runner ./

FROM alpine:3.19
RUN apk add --no-cache ca-certificates && adduser -D app
USER app
WORKDIR /app
COPY --from=build /out/runner /app/runner
ENTRYPOINT ["/app/runner"]
