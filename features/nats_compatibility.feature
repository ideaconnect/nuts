Feature: NATS server compatibility behavior
  As an operator running different supported NATS versions
  I want multi-topic SSE subscriptions to use the compatible JetStream filter strategy
  So that clients receive only the requested subjects without duplicate delivery

  Background:
    Given a NATS JetStream server is running
    And the stream "EVENTS" exists with subjects "events.>"

  Scenario: Multi-topic subscription uses the server-compatible filter strategy
    Given I am connected to SSE endpoint "/events?topic=matrix.alpha&topic=matrix.beta"
    Then the stream "EVENTS" should have an active consumer using expected multi-topic filters for subjects "events.matrix.alpha,events.matrix.beta"
    When I publish message '{"kind":"alpha"}' to subject "events.matrix.alpha"
    And I publish message '{"kind":"beta"}' to subject "events.matrix.beta"
    And I publish message '{"kind":"gamma"}' to subject "events.matrix.gamma"
    Then I should receive an SSE event with topic "matrix.alpha"
    And I should receive an SSE event with topic "matrix.beta"
    And I should have received 2 SSE message events
    But I should not receive an SSE event with topic "matrix.gamma"

  Scenario: Multi-topic stream subject mismatch returns a clear error
    Given the stream "EVENTS" exists with subjects "events.allowed.>"
    When I request SSE endpoint "/events?topic=allowed.one&topic=blocked.one"
    Then I should receive HTTP status 503
    And the response should contain "Failed to subscribe to requested topics: blocked.one"
