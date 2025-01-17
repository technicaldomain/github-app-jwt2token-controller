FROM golang:1.23.4-alpine3.21 AS builder

WORKDIR /app
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -a -installsuffix cgo -o github-app-jwt2token-controller .

FROM scratch
COPY --from=builder /app/github-app-jwt2token-controller /app/github-app-jwt2token-controller
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/app/github-app-jwt2token-controller"]
