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
