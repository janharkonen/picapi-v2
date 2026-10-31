# Build context: repository root (docker build -f web/Dockerfile .)

FROM node:24-slim AS build
WORKDIR /app
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build && npm prune --omit=dev

FROM node:24-slim
WORKDIR /app
COPY --from=build /app/build ./build
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/package.json ./
# HOST=:: listens on IPv4 and IPv6, which Railway's private network may use.
ENV NODE_ENV=production PORT=3000 HOST=::
EXPOSE 3000
CMD ["node", "build"]
