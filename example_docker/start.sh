#!/bin/sh
cd "$(dirname "$0")"
if docker compose up --help 2>/dev/null | grep -q -- --wait; then
	docker compose up -d --wait
else
	docker compose up -d
fi

echo ""
echo "========================================"
echo "  NUTS is ready!"
echo "  Open: http://localhost:8080"
echo "========================================"
echo ""
