Feature: JetStream consumer recovery
  As an operator
  I want an SSE stream to survive the loss of its JetStream consumer
  So that clients keep receiving messages without a gap or a reconnect

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  # A consumer can disappear under a live stream: reaped by
  # InactiveThreshold during an outage, lost with a leafnode route, or
  # deleted by an operator. Caddyfile.test sets nats_idle_heartbeat 1, so
  # the ordered consumer notices the missing heartbeats within about two
  # seconds and recreates itself after the last delivered message.
  Scenario: SSE stream recovers after the JetStream consumer is deleted
    Given I am connected to SSE endpoint "/events?topic=invalidation"
    And I note the value of metric 'nuts_consumer_invalidated_total{reason="recreated"}'
    When I publish message '{"phase":"baseline"}' to subject "events.invalidation"
    Then I should receive an SSE event containing 'baseline'
    When I delete the active JetStream consumer for stream "EVENTS"
    And I publish message '{"phase":"after-delete"}' to subject "events.invalidation"
    Then I should receive an SSE event containing 'after-delete'
    And the SSE stream should still be open
    And the received message event ids should be contiguous
    And the metric 'nuts_consumer_invalidated_total{reason="recreated"}' should have increased

  # A restarted server has forgotten every consumer: NUTS' ordered consumers
  # keep their state in the server's memory. Each stream recreates its
  # consumer after the last message it delivered; on disk, the messages
  # outlive the restart.
  Scenario Outline: An SSE stream survives a NATS restart without a hole
    Given the stream "EVENTS" exists on disk with subjects "events.>"
    And I am connected to SSE endpoint "<endpoint>"
    When I publish 3 messages to subject "events.restart"
    Then I should have received 3 SSE message events
    When NATS restarts
    And I publish 3 messages to subject "events.restart"
    Then I should have received 6 SSE message events
    And the received message event ids should be contiguous
    And the SSE stream should still be open

    Examples:
      | endpoint              |
      | /events?topic=restart |
      | /shared?topic=restart |

  # A stream deleted and created again numbers its messages from 1. An open
  # SSE stream's ordered consumer would recreate itself after the last
  # sequence it delivered and wait there, skipping the new stream's first
  # messages, so NUTS ends the SSE stream instead (#133). Its last frame, a
  # reset event, sets the client's last event ID to 0, and the reconnect
  # replays the new stream from its start. Client B's request reads the
  # stream and notices the new one at once; without requests, NUTS reads it
  # every 10 seconds.
  Scenario Outline: A recreated stream sends its clients to the new one
    Given client "A" is connected to SSE endpoint "<endpoint>"
    When I publish 3 messages to subject "events.recreate"
    Then client "A" should have received 3 messages
    When the stream "EVENTS" is deleted and created again with subjects "events.>"
    And I publish 5 messages to subject "events.recreate"
    And client "B" is connected to SSE endpoint "<endpoint>"
    Then the SSE stream of client "A" should end within 5 seconds
    And client "A" should have received a "reset" event
    And the last event ID of client "A" should be "0"
    When client "A" reconnects to SSE endpoint "<endpoint>" with its last event ID
    Then client "A" should have received 5 messages
    And client "B" should have received 0 messages

    Examples:
      | endpoint               |
      | /events?topic=recreate |
      | /shared?topic=recreate |
