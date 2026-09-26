Feature: Shared subscriptions
  As an operator with many clients on the same topics
  I want live connections to share one JetStream consumer
  So that each message crosses the NATS link and is formatted once

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  Scenario: Live clients share one consumer and each receives every message
    Given client "first" is connected to SSE endpoint "/shared?topic=sharedlive"
    And client "second" is connected to SSE endpoint "/shared?topic=sharedlive"
    And client "third" is connected to SSE endpoint "/shared?topic=sharedlive"
    Then the stream "EVENTS" should have 1 consumer
    When I publish message '{"n":1}' to subject "events.sharedlive"
    And I publish message '{"n":2}' to subject "events.sharedlive"
    And I publish message '{"n":3}' to subject "events.sharedlive"
    Then client "first" should have received 3 messages
    And client "second" should have received 3 messages
    And client "third" should have received 3 messages

  Scenario: A reconnecting client replays on its own consumer, then joins
    Given client "live" is connected to SSE endpoint "/shared?topic=sharedreplay"
    And client "returning" is connected to SSE endpoint "/shared?topic=sharedreplay"
    When I publish message '{"n":1}' to subject "events.sharedreplay"
    Then client "returning" should have received 1 messages
    When client "returning" disconnects
    And I publish message '{"n":2}' to subject "events.sharedreplay"
    And I publish message '{"n":3}' to subject "events.sharedreplay"
    And client "returning" reconnects to SSE endpoint "/shared?topic=sharedreplay" with its last event ID
    Then client "returning" should have received 3 messages in total
    And client "returning" should have received an event containing '"n":3'
    And the stream "EVENTS" should have 1 consumer
    When I publish message '{"n":4}' to subject "events.sharedreplay"
    Then client "returning" should have received 4 messages in total
    And client "live" should have received 4 messages
