#!/bin/bash
set -e

NETWORK="realty-net"
APP_IMAGE="subscription-handler"
APP_CONTAINER="subscription-handler"
LOG_DIR="/tmp/subscription-handler-logs"

echo "==> Building image: $APP_IMAGE"
docker build -t "$APP_IMAGE" .

echo "==> Stopping existing container (if any)"
docker rm -f "$APP_CONTAINER" 2>/dev/null || true

echo "==> Creating log directory: $LOG_DIR"
mkdir -p "$LOG_DIR"

echo "==> Starting container: $APP_CONTAINER"
docker run -d \
  --name "$APP_CONTAINER" \
  --network "$NETWORK" \
  --restart unless-stopped \
  -p 9094:9094 \
  -v "$(pwd)/config.yaml:/app/config.yaml:ro" \
  -v "$(pwd)/bot_token:/etc/subscription-handler/bot_token:ro" \
  -v "$LOG_DIR:/var/log/subscription-handler" \
  "$APP_IMAGE"

echo ""
echo "Useful commands:"
echo "  Logs:    docker logs -f $APP_CONTAINER"
echo "  Metrics: curl http://localhost:9094/metrics"
echo "  Health:  curl http://localhost:9094/healthz"
echo "  Stop:    docker stop $APP_CONTAINER"
