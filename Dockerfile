FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY cmd ./cmd
RUN go build -o /bin/printspooler ./cmd/printspooler

FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /bin/printspooler /usr/local/bin/printspooler
COPY .env.example /app/.env.example
CMD ["printspooler"]
