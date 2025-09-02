FROM golang:1.22 AS build
WORKDIR /src

# 1) Prime the module cache
COPY go.mod ./
COPY go.sum ./
RUN go mod download

# 2) Bring in the source and reconcile deps
COPY . .
RUN go mod tidy

# 3) Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/kioshun-node ./cmd/kioshun-node

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kioshun-node /usr/local/bin/kioshun-node
EXPOSE 5011
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/kioshun-node"]
