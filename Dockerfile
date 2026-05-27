FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o immich-bridge .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/immich-bridge /usr/local/bin/
ENTRYPOINT ["immich-bridge"]
CMD ["daemon"]
