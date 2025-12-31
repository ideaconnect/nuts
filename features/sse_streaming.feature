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

  Scenario: Path-based topic subscription
    Given I am connected to SSE endpoint "/mypath"
    When I publish message '{"path": "based"}' to subject "events.mypath"
    Then I should receive an SSE event with topic "mypath"

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
    When I wait for 2 seconds
    Then I should receive a heartbeat comment
