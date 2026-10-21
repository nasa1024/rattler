FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/rattler .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder /out/rattler /usr/local/bin/rattler
COPY config.example.json /app/config.example.json

ENTRYPOINT ["rattler"]
CMD ["-config", "/app/config.json"]
