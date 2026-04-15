Feature: Multi-client SSE streaming with disconnect and reconnect
  As a system with multiple concurrent clients
  I want each client to receive the correct messages
  Including after disconnects and reconnects using Last-Event-ID

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  Scenario: Three clients with disconnect and reconnect
    # First and second clients connect from the beginning
    Given client "first" is connected to SSE endpoint "/events?topic=multiclient"
    And client "second" is connected to SSE endpoint "/events?topic=multiclient"

    # Publish 6 messages
    When I publish message '{"count":1}' to subject "events.multiclient"
    And I publish message '{"count":2}' to subject "events.multiclient"
    And I publish message '{"count":3}' to subject "events.multiclient"
    And I publish message '{"count":4}' to subject "events.multiclient"
    And I publish message '{"count":5}' to subject "events.multiclient"
    And I publish message '{"count":6}' to subject "events.multiclient"

    # Both clients should have all 6 messages
    Then client "first" should have received 6 messages
    And client "second" should have received 6 messages

    # Second client disconnects
    When client "second" disconnects

    # Publish 4 more messages while second is disconnected
    And I publish message '{"count":7}' to subject "events.multiclient"
    And I publish message '{"count":8}' to subject "events.multiclient"
    And I publish message '{"count":9}' to subject "events.multiclient"
    And I publish message '{"count":10}' to subject "events.multiclient"

    # First client should have all 10
    Then client "first" should have received 10 messages

    # Second client reconnects with its last event ID (should get messages 7-10)
    When client "second" reconnects to SSE endpoint "/events?topic=multiclient" with its last event ID

    # Third client connects using second client's last event ID (should also get messages 7-10)
    And client "third" connects to SSE endpoint "/events?topic=multiclient" with last event ID from client "second"

    # Second client should have all 10 messages in total (6 before disconnect + 4 after reconnect)
    Then client "second" should have received 10 messages in total
    And client "second" should have received an event containing '"count":7'
    And client "second" should have received an event containing '"count":10'

    # Third client should have only the 4 remaining messages
    And client "third" should have received 4 messages
    And client "third" should have received an event containing '"count":7'
    And client "third" should have received an event containing '"count":10'
    But client "third" should not have received an event containing '"count":6'

    # First client should have everything
    And client "first" should have received an event containing '"count":1'
    And client "first" should have received an event containing '"count":10'
