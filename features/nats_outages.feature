Feature: NATS outages
  As an operator
  I want NUTS to notice a NATS server that stops answering
  So that clients are told to retry at once instead of waiting on timeouts

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  # The paused container keeps its connections open and answers nothing.
  # Caddyfile.test sets nats_ping_interval 1: two unanswered pings mark the
  # connection stale a few seconds in, and requests are refused at once from
  # then on. Open streams stay open and carry on once NATS answers again.
  Scenario: A NATS server that stops answering is noticed within seconds
    Given I am connected to SSE endpoint "/events?topic=paused"
    When NATS stops answering
    Then an EventSource request for SSE endpoint "/events?topic=paused" is told to retry within 10 seconds
    When NATS answers again
    And I publish message '{"phase":"after"}' to subject "events.paused"
    Then I should receive an SSE event containing 'after'
    And the SSE stream should still be open
