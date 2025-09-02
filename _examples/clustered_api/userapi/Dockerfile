FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd _examples/mesh_api/userapi && go build -o /out/userapi ./

FROM alpine:3.19
RUN adduser -D app
USER app
WORKDIR /app
COPY --from=build /out/userapi /app/userapi
EXPOSE 8081 8082 8083 5011 5012 5013
ENTRYPOINT ["/app/userapi"]
