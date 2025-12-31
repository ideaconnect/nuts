#!/bin/bash
# Setup script for NUTS development environment
# This script starts the NATS server and creates the required JetStream stream

set -e

echo "🚀 Starting NATS server with JetStream..."
docker compose up -d

echo "⏳ Waiting for NATS to be ready..."
sleep 3

# Check if nats CLI is available
if ! command -v nats &> /dev/null; then
    echo "⚠️  NATS CLI not found. Please install it from https://github.com/nats-io/natscli"
    echo "   Or create the stream manually:"
    echo ""
    echo "   nats stream add EVENTS \\"
    echo "     --server=nats://localhost:4222 \\"
    echo "     --subjects \"events.>\" \\"
    echo "     --storage memory \\"
    echo "     --retention limits \\"
    echo "     --max-msgs 10000 \\"
    echo "     --max-age 1h \\"
    echo "     --discard old"
    exit 0
fi

echo "📦 Creating JetStream stream 'EVENTS'..."
nats stream add EVENTS \
    --server=nats://localhost:4222 \
    --subjects "events.>" \
    --storage memory \
    --retention limits \
    --max-msgs 10000 \
    --max-age 1h \
    --discard old \
    --defaults 2>/dev/null || echo "   Stream already exists or created"

echo ""
echo "✅ NATS server is ready!"
echo ""
echo "   NATS URL:     nats://localhost:4222"
echo "   Monitor:      http://localhost:8222"
echo "   Stream:       EVENTS (subjects: events.>)"
echo ""
echo "   To stop:      docker compose down"
echo "   To view logs: docker compose logs -f"
