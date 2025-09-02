FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd _examples/mesh_api/loadgen && go build -o /out/loadgen ./

FROM alpine:3.19
RUN apk add --no-cache ca-certificates && adduser -D app
USER app
WORKDIR /app
COPY --from=build /out/loadgen /app/loadgen
ENTRYPOINT ["/app/loadgen"]
