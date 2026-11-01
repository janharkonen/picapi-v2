# Build context is the repository root.
FROM golang:1.25 AS build
WORKDIR /src
COPY api/go.mod api/go.sum ./
RUN go mod download
COPY api/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /picapi .

FROM gcr.io/distroless/static-debian12
COPY --from=build /picapi /picapi
ENV GIN_MODE=release
ENTRYPOINT ["/picapi"]
