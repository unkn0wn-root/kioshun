FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    cd _benchmarks; \
    go mod tidy; \
    cd cluster/node && go build -o /out/meshnode ./

FROM alpine:3.19
RUN apk add --no-cache wget && adduser -D app
USER app
WORKDIR /app
COPY --from=build /out/meshnode /app/meshnode
EXPOSE 8081 8082 8083 5011 5012 5013
ENTRYPOINT ["/app/meshnode"]
