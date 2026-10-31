# 服务端镜像（Phase 0）。本机无 Docker，该镜像未经实机构建验证，见 docs/TODO-phase0.md。
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/server /app/server
COPY migrations /app/migrations
EXPOSE 8080
ENTRYPOINT ["/app/server", "-store", "postgres", "-migrate", "-seed"]
