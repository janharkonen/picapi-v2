# Build context: repository root (docker build -f api-go/Dockerfile .)

FROM golang:1.26-trixie AS build
RUN apt-get update && apt-get install -y --no-install-recommends libvips-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY api-go/go.mod api-go/go.sum ./
RUN go mod download
COPY api-go/ ./
RUN go build -trimpath -ldflags="-s -w" -o /out/picapi-server ./cmd/picapi-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/picapi ./cmd/picapi

# ONNX Runtime (must match the C API version onnxruntime_go is built against) and
# background-removal models from rembg (both Apache-2.0).
FROM debian:trixie-slim AS models
ARG ORT_VERSION=1.31.0
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN case "$(dpkg --print-architecture)" in \
      amd64) ORT_ARCH=x64 ;; arm64) ORT_ARCH=aarch64 ;; *) echo "unsupported arch" && exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}.tgz" \
    | tar -xz -C /tmp \
 && mkdir -p /out/lib /out/models \
 && cp -L /tmp/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so /out/lib/libonnxruntime.so \
 && curl -fsSL -o /out/models/isnet-general-use.onnx https://github.com/danielgatis/rembg/releases/download/v0.0.0/isnet-general-use.onnx \
 && curl -fsSL -o /out/models/u2netp.onnx https://github.com/danielgatis/rembg/releases/download/v0.0.0/u2netp.onnx

FROM debian:trixie-slim
# libheif-plugin-aomenc provides AVIF encoding; dav1d provides AVIF decoding.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libvips42t64 libheif-plugin-aomenc libheif-plugin-dav1d ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=models /out/lib/libonnxruntime.so /usr/local/lib/libonnxruntime.so
COPY --from=models /out/models /opt/picapi/models
COPY --from=build /out/picapi-server /out/picapi /usr/local/bin/
ENV PORT=8080
EXPOSE 8080
CMD ["picapi-server"]
