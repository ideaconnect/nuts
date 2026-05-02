#!/bin/bash
cd /home/bartosz/dev/idct/nuts
docker compose up -d
echo "Waiting for containers to start..."
sleep 20
docker ps -a
echo "--- nuts-server logs ---"
docker logs nuts-server
echo "--- nuts-nats logs ---"
docker logs nuts-nats
echo "--- running tests ---"
cd functional_test
TEST_BASE_URL=http://localhost:8080 TEST_NATS_URL=nats://localhost:4222 go test -v -timeout 180s ./... 2>&1
EXIT_CODE=$?
cd /home/bartosz/dev/idct/nuts
docker compose down -v
exit $EXIT_CODE
