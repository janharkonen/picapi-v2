FROM golang:1.26-trixie AS build
RUN apt-get update && apt-get install -y --no-install-recommends libvips-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go schema.sql ./
RUN go build -trimpath -ldflags="-s -w" -o /picapi .

FROM debian:trixie-slim
# libheif-plugin-aomenc provides AVIF encoding; dav1d provides AVIF decoding.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libvips42t64 libheif-plugin-aomenc libheif-plugin-dav1d ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /picapi /usr/local/bin/picapi
ENV PORT=8080 DATABASE_PATH=/data/picapi.db
VOLUME /data
EXPOSE 8080
CMD ["picapi"]
