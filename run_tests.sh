#!/bin/bash
cd /home/bartosz/dev/idct/nuts
make docker-up
if [ $? -ne 0 ]; then
    echo "docker-up failed, showing logs..."
    docker ps -a
    docker logs nuts-server
fi

cd functional_test
TEST_BASE_URL=http://localhost:8080 TEST_NATS_URL=nats://localhost:4222 go test -v -timeout 180s ./... 2>&1
EXIT_CODE=$?

cd /home/bartosz/dev/idct/nuts
make docker-down
exit $EXIT_CODE
