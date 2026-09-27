Feature: SSE Streaming with JetStream
  As a client application
  I want to receive real-time messages via SSE
  So that I can react to events as they happen

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  Scenario: Connect to SSE endpoint and receive messages
    Given I am connected to SSE endpoint "/events?topic=notifications"
    When I publish message '{"alert": "test"}' to subject "events.notifications"
    Then I should receive an SSE event with topic "notifications"
    And the event payload should contain "alert"
    And the event should have an ID

  # event_type topic names each event after its topic (#142), so a page can
  # add one listener per topic instead of routing every message itself.
  Scenario: Events are named after their topic
    Given I am connected to SSE endpoint "/typed?topic=orders"
    When I publish message '{"id": 1}' to subject "events.orders"
    Then I should receive a "orders" event
    And I should not receive an SSE event with topic "notifications"

  # payload_format raw sends the NATS payload itself as the event's data,
  # without the JSON envelope (#143).
  Scenario: Events carry the raw payload
    Given I am connected to SSE endpoint "/raw?topic=plain"
    When I publish message 'just some text' to subject "events.plain"
    Then I should receive an SSE event containing 'just some text'
    And I should not receive an SSE event containing '"payload"'

  Scenario: Receive messages from multiple topics
    Given I am connected to SSE endpoint "/events?topic=alerts&topic=updates"
    When I publish message '{"type": "alert"}' to subject "events.alerts"
    And I publish message '{"type": "update"}' to subject "events.updates"
    Then I should receive an SSE event with topic "alerts"
    And I should receive an SSE event with topic "updates"

  Scenario: Replay messages using last-id parameter
    Given I publish message '{"seq": 1}' to subject "events.replay"
    And I publish message '{"seq": 2}' to subject "events.replay"
    And I publish message '{"seq": 3}' to subject "events.replay"
    When I connect to SSE endpoint "/events?topic=replay" with last-id from message 1
    Then I should receive an SSE event containing '"seq":2'
    And I should receive an SSE event containing '"seq":3'
    But I should not receive an SSE event containing '"seq":1'

  Scenario: Last-Event-ID header wins over the last-id parameter
    # EventSource resends its original URL, ?last-id= included, on every
    # reconnect; the fresher header must decide where the replay starts.
    Given I publish message '{"seq": 1}' to subject "events.precedence"
    And I publish message '{"seq": 2}' to subject "events.precedence"
    And I publish message '{"seq": 3}' to subject "events.precedence"
    When I connect to SSE endpoint "/events?topic=precedence&last-id=0" with Last-Event-ID from message 2
    Then I should receive an SSE event containing '"seq":3'
    But I should not receive an SSE event containing '"seq":2'
    And I should not receive an SSE event containing '"seq":1'

  Scenario Outline: Probe paths accept a trailing slash
    When I request SSE endpoint "<path>"
    Then I should receive HTTP status 200
    And the response header "Content-Type" should be "application/json"
    And the response should contain "<body>"

    Examples:
      | path             | body      |
      | /events/livez/   | ok        |
      | /events/readyz/  | available |
      | /events/healthz/ | connected |

  # health_details adds the NATS server and the stream's details to the
  # readiness probes (#144); without it they stay terse.
  Scenario: Readiness details are opt-in
    When I request SSE endpoint "/details/readyz"
    Then I should receive HTTP status 200
    And the response should contain "nats_server"
    And the response should contain "version"
    And the response should contain "stream_info"
    And the response should contain "EVENTS"
    When I request SSE endpoint "/events/readyz"
    Then I should receive HTTP status 200
    And the response should not contain "nats_server"

  Scenario: A backlog larger than the prefetch replays on one connection
    # client_buffer_size is 64 by default; the stream stops pulling while the
    # client catches up instead of disconnecting it.
    Given I publish 200 messages to subject "events.backlog"
    When I connect to SSE endpoint "/events?topic=backlog&last-id=0"
    Then I should have received 200 SSE message events
    And the received message event ids should be contiguous
    And the SSE stream should still be open

  Scenario: A burst larger than the prefetch reaches a live client without a slow-client disconnect
    # 500 messages arrive at once, far more than client_buffer_size (64): the
    # stream stops pulling while the client catches up instead of dropping
    # the client.
    Given I am connected to SSE endpoint "/events?topic=burst"
    And I note the value of metric 'nuts_slow_client_disconnects_total'
    When I publish 500 messages to subject "events.burst" at once
    Then I should have received 500 SSE message events
    And the received message event ids should be contiguous
    And the SSE stream should still be open
    And the metric 'nuts_slow_client_disconnects_total' should not have changed

  Scenario: Path-based topic subscription
    Given I am connected to SSE endpoint "/mypath"
    When I publish message '{"path": "based"}' to subject "events.mypath"
    Then I should receive an SSE event with topic "mypath"

  Scenario: Stream responses carry the SSE headers proxies rely on
    When I connect to SSE endpoint "/events?topic=headers"
    Then the SSE response header "Content-Type" should be "text/event-stream"
    And the SSE response header "Cache-Control" should be "no-cache"
    And the SSE response header "X-Accel-Buffering" should be "no"

  Scenario: Receive connected event on connection
    When I connect to SSE endpoint "/events?topic=test"
    Then I should receive a "connected" event
    And the connected event should list topic "test"

  Scenario: Invalid last-id parameter returns error
    When I request SSE endpoint "/events?topic=test&last-id=invalid"
    Then I should receive HTTP status 400
    And the response should contain "Invalid last-id"

  Scenario: No topics specified returns error
    When I request SSE endpoint "/"
    Then I should receive HTTP status 400
    And the response should contain "No topics specified"

  Scenario: CORS preflight request
    When I send OPTIONS request to "/events?topic=test" with origin "https://example.com"
    Then I should receive HTTP status 204
    And the response header "Access-Control-Allow-Origin" should be "https://example.com"

  Scenario: Heartbeat keeps connection alive
    Given I am connected to SSE endpoint "/events?topic=heartbeat"
    Then I should receive a heartbeat comment
