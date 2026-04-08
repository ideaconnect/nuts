<?php

/**
 * NUTS Demo Sender
 *
 * Publishes a random number (0-100) to "events.numbers" and a random
 * letter (A-Z) to "events.letters" every 250 ms using the raw NATS
 * protocol over a TCP socket. No external libraries required.
 */

$natsHost = getenv('NATS_HOST') ?: 'nats';
$natsPort = (int)(getenv('NATS_PORT') ?: 4222);

echo "Connecting to NATS at {$natsHost}:{$natsPort}...\n";

$sock = @fsockopen($natsHost, $natsPort, $errno, $errstr, 5);
if (!$sock) {
    fwrite(STDERR, "Failed to connect to NATS: [{$errno}] {$errstr}\n");
    exit(1);
}

// Read server INFO line
$info = fgets($sock);
if ($info === false || strpos($info, 'INFO') !== 0) {
    fwrite(STDERR, "Unexpected response from NATS server\n");
    exit(1);
}

// Send CONNECT
fwrite($sock, "CONNECT {\"verbose\":false,\"pedantic\":false,\"lang\":\"php\",\"version\":\"1.0.0\"}\r\n");
fwrite($sock, "PING\r\n");
fflush($sock);

// Wait for PONG to confirm connection
$pong = fgets($sock);
if ($pong === false || strpos(trim($pong), 'PONG') !== 0) {
    fwrite(STDERR, "NATS handshake failed\n");
    exit(1);
}

echo "Connected! Publishing every 250ms...\n";

function natsPublish($sock, string $subject, string $payload): void
{
    $len = strlen($payload);
    fwrite($sock, "PUB {$subject} {$len}\r\n{$payload}\r\n");
}

while (true) {
    $number = random_int(0, 100);
    $letter = chr(random_int(65, 90)); // A-Z

    $numberPayload = json_encode(['number' => $number]);
    $letterPayload = json_encode(['letter' => $letter]);

    natsPublish($sock, 'events.numbers', $numberPayload);
    natsPublish($sock, 'events.letters', $letterPayload);
    fflush($sock);

    echo "Published: number={$number}, letter={$letter}\n";

    usleep(250000); // 250ms
}
