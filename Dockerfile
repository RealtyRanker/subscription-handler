FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /subscription-handler ./cmd/handler

# ---

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata
RUN mkdir -p /var/log/subscription-handler

WORKDIR /app
COPY --from=builder /subscription-handler .
COPY config.yaml .

EXPOSE 9090

CMD ["./subscription-handler", "-config", "config.yaml"]
