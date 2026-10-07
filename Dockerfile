FROM node:24 AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
COPY internal/web/dist/README.md /src/internal/web/dist/README.md
RUN pnpm build

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist/app ./internal/web/dist/app
RUN CGO_ENABLED=0 go build -o /out/engram-appview ./cmd/engram-appview && \
    CGO_ENABLED=0 go build -o /out/engram-mcp ./cmd/engram-mcp && \
    CGO_ENABLED=0 go build -o /out/engram-config ./cmd/engram-config && \
    CGO_ENABLED=0 go build -o /out/engram-web ./cmd/engram-web

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080 8090
# The appview by default; run the web app with --entrypoint engram-web.
ENTRYPOINT ["/usr/local/bin/engram-appview"]
