#!/bin/sh
cd "$(dirname "$0")"
# The demo shows the latest release: fetch it instead of running whatever
# copy of idcttech/nuts:latest was pulled before.
docker compose pull nuts
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
