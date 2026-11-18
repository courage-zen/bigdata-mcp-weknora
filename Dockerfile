FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/weknora-mcp .

FROM alpine:3.20
RUN adduser -D -u 10001 mcp
USER mcp
COPY --from=build /out/weknora-mcp /usr/local/bin/weknora-mcp
EXPOSE 8000
ENTRYPOINT ["weknora-mcp"]
