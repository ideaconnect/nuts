<?php

/**
 * NUTS Demo Sender
 *
 * Publishes a random number (0-100) to "events.numbers" and a random
 * letter (A-Z) to "events.letters" every 250 ms using
 * idct/php-nats-jetstream-client for JetStream-backed publishing.
 */

require __DIR__ . '/vendor/autoload.php';

use IDCT\NATS\Connection\NatsOptions;
use IDCT\NATS\Core\NatsClient;

$natsHost = getenv('NATS_HOST') ?: 'nats';
$natsPort = (int)(getenv('NATS_PORT') ?: 4222);
$natsUrl  = "nats://{$natsHost}:{$natsPort}";

echo "Connecting to NATS at {$natsUrl}...\n";

$client = new NatsClient(new NatsOptions(servers: [$natsUrl]));
$client->connect()->await();

$js = $client->jetStream();

echo "Connected! Publishing every 250ms...\n";

while (true) {
    $number = random_int(0, 100);
    $letter = chr(random_int(65, 90)); // A-Z

    $numberPayload = json_encode(['number' => $number]);
    $letterPayload = json_encode(['letter' => $letter]);

    $js->publish('events.numbers', $numberPayload)->await();
    $js->publish('events.letters', $letterPayload)->await();

    echo "Published: number={$number}, letter={$letter}\n";

    usleep(50000); // 50ms
}
