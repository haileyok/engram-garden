FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/engram-appview ./cmd/engram-appview && \
    CGO_ENABLED=0 go build -o /out/engram-mcp ./cmd/engram-mcp

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/engram-appview"]
