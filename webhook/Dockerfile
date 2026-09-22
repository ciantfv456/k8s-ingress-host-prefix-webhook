# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/ingress-host-prefix-webhook .

FROM alpine:3.19

RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 webhook

COPY --from=builder /out/ingress-host-prefix-webhook /usr/local/bin/ingress-host-prefix-webhook

USER 10001

EXPOSE 8443
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/ingress-host-prefix-webhook"]
