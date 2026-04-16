#!/bin/sh
cd "$(dirname "$0")"
docker compose up -d --wait

echo ""
echo "========================================"
echo "  NUTS is ready!"
echo "  Open: http://localhost:8080"
echo "========================================"
echo ""
